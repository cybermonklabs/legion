import { beforeAll, expect, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { type ReactNode, useMemo } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import type { DisplayTree } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { outlineRowPath } from "./lenses/store-link";
import { NoTooltips } from "./NoTooltips";
import { waitInAct } from "./outline-fixtures";
import { RowText } from "./RowText";
import { ReferenceNamesProvider, usePublishReferenceNames } from "./reference-names";

/**
 * A row's words keep the link the reader is on through a read of the tree again. A later read
 * hands a row the same words in objects of its own, which paint nothing again; where the words do
 * change, the new link of the same address takes the focus back; and a reader who put the focus
 * on nothing is given none.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The names the page's one batched read brought, published as the page publishes them. */
function Named({ children }: { children: ReactNode }): ReactNode {
  const names = useMemo(() => new Map([["dispatch://CORE-2", "Draw the children"]]), []);
  usePublishReferenceNames(names);
  return children;
}

/** The row's words `markdown`, drawn from the reading `tree` of them. */
function row(markdown: string, tree: DisplayTree | undefined): ReactNode {
  return (
    <ReferenceNamesProvider>
      <Named>
        <RowText markdown={markdown} tree={tree} />
      </Named>
    </ReferenceNamesProvider>
  );
}

/** Draws `children` as the page's slots draw a row's parts, inside `NoTooltips`, on the page of
 *  the issue CORE-1; `redraw` draws them again in the same tree, as a read of the tree again
 *  does. */
function draw(children: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const page = (inside: ReactNode) => (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/issues/CORE-1"]}>
        <NoTooltips>{inside}</NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(page(children));
  return { ...view, redraw: (inside: ReactNode) => view.rerender(page(inside)) };
}

const CORE_2 = 'a[data-row-reference="dispatch://CORE-2"]';

/** The row's own link to CORE-2 in `container`, once the row's link pass has made it, with the
 *  focus put on it as the row's `f` puts it there. */
async function focusedLink(container: HTMLElement): Promise<HTMLAnchorElement> {
  const link = await waitFor(() => {
    const found = container.querySelector<HTMLAnchorElement>(CORE_2);
    expect(found).not.toBeNull();
    return found as HTMLAnchorElement;
  });
  act(() => link.focus());
  expect(document.activeElement).toBe(link);
  return link;
}

test("a row's words drawn again from the same words in new objects keep the focused link", async () => {
  const text = "Draw the outline as CORE-2 says.";
  const reading = displayTree(text);
  const view = draw(row(text, reading));
  try {
    const link = await focusedLink(view.container);
    // A later read of the tree answers the same reading in objects of its own. Each reading the
    // test's engine makes gives its blocks ids of their own, so the copy is of the one reading.
    view.redraw(row(text, structuredClone(reading)));
    await waitInAct(0);
    expect(link.isConnected, "the same words drawn again replaced the link").toBe(true);
    expect(document.activeElement, "the same words drawn again took the focus").toBe(link);
  } finally {
    view.unmount();
  }
});

test("a row whose words change keeps the focus on the link of the same address", async () => {
  const text = "Draw the outline as CORE-2 says.";
  const changed = `${text} Then draw the margin.`;
  const view = draw(row(text, displayTree(text)));
  try {
    await focusedLink(view.container);
    view.redraw(row(changed, displayTree(changed)));
    await waitFor(() => expect(view.container.textContent).toContain("Then draw the margin."));
    await waitInAct(0);
    const focused = document.activeElement;
    expect(focused?.isConnected, "the focus is on a link the paint removed").toBe(true);
    expect(view.container.contains(focused), "the focus left the row's words").toBe(true);
    expect(focused?.matches(CORE_2), "the focus is not on the link to CORE-2").toBe(true);
  } finally {
    view.unmount();
  }
});

test("a reader who put the focus on nothing is given none by a paint of other words", async () => {
  const text = "Draw the outline as CORE-2 says.";
  const changed = `${text} Then draw the margin.`;
  const view = draw(row(text, displayTree(text)));
  try {
    const link = await focusedLink(view.container);
    // The reader leaves the link for nothing, as a press on the page's blank space does.
    act(() => link.blur());
    await waitInAct(0);
    expect(document.activeElement, "the link kept the focus the reader took off it").toBe(
      document.body
    );
    view.redraw(row(changed, displayTree(changed)));
    await waitFor(() => expect(view.container.textContent).toContain("Then draw the margin."));
    await waitInAct(0);
    expect(document.activeElement, "a paint of other words gave the focus back").toBe(
      document.body
    );
  } finally {
    view.unmount();
  }
});

// A store address the words name by its node is a link holding the address, which the body's
// reference pass leaves alone, and its row's path as its title. The row makes the path its
// address: the link opens that row, with the node's name as its words, and is no reference link.
test("a store address named in a row's words links to its node's row by the node's name", async () => {
  const address = "dispatch://risk/d4000000-0000-4000-8000-000000000004";
  const path = "/issues/CORE-1?node=d4000000-0000-4000-8000-000000000004";
  const heading = "Failover takes minutes";
  const reading: DisplayTree = {
    content: [
      {
        content: [
          { text: "Two regions, until ", type: "text" },
          {
            marks: [{ attrs: { href: address, title: path }, type: "link" }],
            text: heading,
            type: "text",
          },
          { text: ".", type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const view = draw(row(`Two regions, until ${address}.`, reading));
  try {
    await waitFor(() => {
      const links = [...view.container.querySelectorAll("a")].filter(
        (anchor) => anchor.textContent === heading
      );
      expect(links.map((link) => link.getAttribute("href"))).toEqual([path]);
      expect(links[0]?.hasAttribute("title"), "the link keeps the row's path as its title").toBe(
        false
      );
      expect(links[0]?.hasAttribute("data-dispatch-ref"), "the link is a reference link").toBe(
        false
      );
    });
  } finally {
    view.unmount();
  }
});

// A link to a store address takes its title as its address only where the title is the very path
// a name's link holds: the outline of the page's issue at the `?node=` of that address's node
// (`lenses/store-link.ts`' `storeLinkPath`). An author's own title on such a link - another
// issue's page, or this issue's at another node - is no address, and the link opens nothing.
test("a link to a store address opens no path an author wrote as its title", async () => {
  const address = "dispatch://risk/d4000000-0000-4000-8000-000000000004";
  const path = "/issues/CORE-1?node=d4000000-0000-4000-8000-000000000004";
  const linked = (text: string, title: string): DisplayTree => ({
    marks: [{ attrs: { href: address, title }, type: "link" }],
    text,
    type: "text",
  });
  const reading: DisplayTree = {
    content: [
      {
        content: [
          linked("Failover takes minutes", path),
          { text: ", ", type: "text" },
          linked("Approve", "/issues/OTHER-1?node=x"),
          { text: ", ", type: "text" },
          linked("Approve here", "/issues/CORE-1?node=e5000000-0000-4000-8000-000000000005"),
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const view = draw(row(`${address}, [Approve](${address}), [Approve here](${address})`, reading));
  try {
    await waitFor(() => {
      const hrefs = [...view.container.querySelectorAll("a")].map((anchor) => [
        anchor.textContent,
        anchor.getAttribute("href"),
      ]);
      expect(hrefs).toEqual([
        ["Failover takes minutes", path],
        ["Approve", ""],
        ["Approve here", ""],
      ]);
    });
  } finally {
    view.unmount();
  }
});

// A store link to a finding names the finding's row by its read id, which `storeAddressOf` builds
// the finding's address from: the lens's id and the block id after a `#`, or the block id alone.
// Both open the row; a read id whose part before its `#` is another lens's opens nothing.
test("a store link to a finding opens its row by either form of its read id", async () => {
  const lens = "c3000000-0000-4000-8000-000000000003";
  const address = `dispatch://lens/${lens}#b-first`;
  const linked = (text: string, readId: string): DisplayTree => ({
    marks: [{ attrs: { href: address, title: outlineRowPath("CORE-1", readId) }, type: "link" }],
    text,
    type: "text",
  });
  const reading: DisplayTree = {
    content: [
      {
        content: [
          linked("By its block", "first"),
          { text: ", ", type: "text" },
          linked("By its lens and block", `lens/${lens}#first`),
          { text: ", ", type: "text" },
          linked("By another lens", "lens/b2000000-0000-4000-8000-000000000002#first"),
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const view = draw(row(`${address}, ${address}, ${address}`, reading));
  try {
    await waitFor(() => {
      const hrefs = [...view.container.querySelectorAll("a")].map((anchor) => [
        anchor.textContent,
        anchor.getAttribute("href"),
      ]);
      expect(hrefs).toEqual([
        ["By its block", outlineRowPath("CORE-1", "first")],
        ["By its lens and block", outlineRowPath("CORE-1", `lens/${lens}#first`)],
        ["By another lens", ""],
      ]);
    });
  } finally {
    view.unmount();
  }
});

// A text with no reading of the server's that names a node is drawn from one paragraph of its
// plain words, the name's link among them and its other addresses as written: the row's link
// pass links those, as it links a text drawn as stored.
test("a block address beside a named store address is linked by the row's link pass", async () => {
  const address = "dispatch://risk/d4000000-0000-4000-8000-000000000004";
  const path = "/issues/CORE-1?node=d4000000-0000-4000-8000-000000000004";
  const block = "dispatch://CORE-1/spec#b-r1";
  const reading: DisplayTree = {
    content: [
      {
        content: [
          { text: "See ", type: "text" },
          {
            marks: [{ attrs: { href: address, title: path }, type: "link" }],
            text: "Failover takes minutes",
            type: "text",
          },
          { text: ` and ${block} first.`, type: "text" },
        ],
        type: "paragraph",
      },
    ],
    type: "doc",
  };
  const view = draw(row(`See ${address} and ${block} first.`, reading));
  try {
    await waitFor(() => {
      const store = [...view.container.querySelectorAll("a")].find(
        (anchor) => anchor.textContent === "Failover takes minutes"
      );
      expect(store?.getAttribute("href")).toBe(path);
      const linked = view.container.querySelector(`a[data-row-reference="${block}"]`);
      expect(linked?.getAttribute("href") ?? "", "the block address is no link").not.toBe("");
    });
  } finally {
    view.unmount();
  }
});
