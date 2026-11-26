import { beforeAll, expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { type ReactNode, useMemo } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { ApiError, api } from "../../api/client";
import type { DisplayTree } from "../../api/types";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ReferenceNamesProvider, usePublishReferenceNames } from "../refs/reference-names";
import { outlineRowPath } from "./lenses/store-link";
import { NoTooltips } from "./NoTooltips";
import { waitInAct } from "./outline-fixtures";
import { publishedNames } from "./projection-fixture";
import { RowText } from "./RowText";

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
  const names = useMemo(
    () =>
      publishedNames(
        new Map([
          ["dispatch://CORE-2", "Draw the children"],
          ["dispatch://CORE-1/spec#b-r1", "Read the page first"],
        ])
      ),
    []
  );
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
      // A link whose title is no row's path is placed as no link: the body draws it as its
      // words, as it draws any link to an address no route names.
      expect(hrefs).toEqual([["Failover takes minutes", path]]);
      expect(view.container.textContent).toBe("Failover takes minutes, Approve, Approve here");
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
      ]);
      expect(view.container.textContent).toBe(
        "By its block, By its lens and block, By another lens"
      );
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
  // The row names the block address by its own read in the commit before the page's names reach
  // the store, which reads the spec's blocks.
  const blocks = spyOn(api, "getReferences").mockResolvedValue({ edges: [] } as never);
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
    blocks.mockRestore();
  }
});

/** The declarations of each rule of `styles.css` whose selectors match `element`. A unit test's
 *  DOM never loads the stylesheet, so a case that reads what the page's rules do to an element
 *  reads them from the file; a selector the DOM cannot match, as one naming a pseudo-element,
 *  matches nothing. */
function stylesheetDeclarations(element: Element): ReadonlySet<string>[] {
  const css = readFileSync(join(import.meta.dir, "..", "..", "styles.css"), "utf8").replace(
    /\/\*[\s\S]*?\*\//g,
    ""
  );
  return [...css.matchAll(/([^{};]+)\{([^{}]*)\}/g)].flatMap(([, selectors = "", body = ""]) => {
    let matched = false;
    try {
      matched = element.matches(selectors.trim());
    } catch {
      matched = false;
    }
    return matched ? [new Set(body.split(";").map((one) => one.replace(/\s+/g, " ").trim()))] : [];
  });
}

/** What keeps `element`'s words from breaking, or lets them break anywhere, from `element` up to
 *  the row's words: the rules of `styles.css` that match each element and each one's classes. */
function breaksOf(element: Element): { anywhere: boolean; nowrap: string[] } {
  const nowrap: string[] = [];
  let anywhere = false;
  for (let at: Element | null = element; at !== null; at = at.parentElement) {
    const declared = stylesheetDeclarations(at);
    const name = at.getAttribute("data-row-reference") ?? at.localName;
    if (declared.some((one) => one.has("white-space: nowrap"))) nowrap.push(`${name}'s rule`);
    if (at.classList.contains("whitespace-nowrap")) nowrap.push(`${name}'s class`);
    if (
      declared.some((one) => one.has("overflow-wrap: anywhere")) ||
      at.classList.contains("[overflow-wrap:anywhere]")
    ) {
      anywhere = true;
    }
    if (at.hasAttribute("data-node-words")) break;
  }
  return { anywhere, nowrap };
}

/** `children` under the names store, with `gone` published as references the page's batched
 *  read answered gone. */
function Gone({ children, gone }: { children: ReactNode; gone: readonly string[] }): ReactNode {
  const names = useMemo(() => publishedNames(new Map(), { gone: new Set(gone) }), [gone]);
  usePublishReferenceNames(names);
  return children;
}

// A reference the page cannot name - a comment that is gone, an issue that does not exist or that
// the reader may not see, which the batched read answers gone - is drawn as the words its author
// wrote, and whatever the row draws it with breaks inside the row, so a written address of some
// 60 characters does not widen the page at 390 px. A unit test's DOM lays nothing out, so this
// reads what the break rests on: no rule of `styles.css` and no class keeps the reference's
// element, or anything around it in the row's words, on one line, and a word there may break
// anywhere. The widths are the browser suite's (`e2e/issue-page-t-unnamed.e2e.ts`).
test("a reference the page cannot name is drawn as written, in an element that breaks inside the row", async () => {
  const comment = "dispatch://CORE-1/comment/1df0a7c8-5b2e-4c1a-9e3d-7f6a2b8c4d01";
  const issue = "dispatch://CORE-404";
  const text = `The answer stood in ${comment}, which ${issue} quoted.`;
  const gone = [comment, issue];
  // In the commit before the page's names reach the store the row reads each reference on its own,
  // and the server answers both as it answers any node that is not there.
  const comments = spyOn(api, "getComment").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no such comment" })
  );
  const summaries = spyOn(api, "getIssueSummary").mockRejectedValue(
    new ApiError(404, { code: "NOT_FOUND", error: "no issue CORE-404" })
  );
  const view = draw(
    <ReferenceNamesProvider>
      <Gone gone={gone}>
        <div data-node-words="">
          <RowText markdown={text} tree={displayTree(text)} />
        </div>
      </Gone>
    </ReferenceNamesProvider>
  );
  try {
    const drawn = await waitFor(() => {
      const chip = view.container.querySelector(`a[data-row-reference="${comment}"]`);
      const key = view.container.querySelector(`a[data-row-reference="${issue}"]`);
      expect(chip?.textContent, "the gone comment is not drawn as written").toBe(comment);
      expect(key?.textContent, "the issue that does not exist is not drawn as written").toBe(issue);
      return { chip: chip as Element, key: key as Element };
    });
    expect(drawn.chip.hasAttribute("data-row-chip"), "the gone comment is no chip in place").toBe(
      true
    );
    for (const element of [drawn.chip, drawn.key]) {
      const reference = element.getAttribute("data-row-reference");
      expect(breaksOf(element), `${reference} is kept on one line`).toEqual({
        anywhere: true,
        nowrap: [],
      });
    }
  } finally {
    view.unmount();
    comments.mockRestore();
    summaries.mockRestore();
  }
});
