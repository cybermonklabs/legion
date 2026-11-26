import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ReferenceNamesProvider } from "../refs/reference-names";
import { NoTooltips } from "./NoTooltips";
import { RowText } from "./RowText";
import { wordsScrollSheet } from "./words-scroll-fixtures";

/**
 * What a row's tab-stop pass reads (decision 503 (b)). `useOutOfTabOrder` takes every element
 * that scrolls its own content out of the tab order, and whether one scrolls is its computed
 * style (`scrolls`). On INFRA-1196 the pass read the computed style of every element of every
 * row's words, 2.1 s of one load (decision 500 (3)). An element that no rule, class or style
 * gives an overflow cannot scroll, so the pass reads the style of none of them.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** A node's words of many plain elements - paragraphs with emphasis, inline code and links, a
 *  list and a quote - and one code block, the one element in them that `styles.css` lets scroll.
 *  Under the 500 nodes past which a body paints in batches (`ATTACH_BATCH`), so it paints at
 *  once. */
const MANY_PLAIN_AND_ONE_PRE = [
  ...Array.from(
    { length: 24 },
    (_unused, at) =>
      `Paragraph ${at + 1} has **strong words**, *quiet words*, \`code ${at + 1}\` and [a link](https://example.com/${at + 1}).`
  ),
  Array.from({ length: 10 }, (_unused, at) => `- item ${at + 1} with **one** word held`).join("\n"),
  "> A quote of *two* words.",
  ["```sh", "bun run e2e --project=chromium", "```"].join("\n"),
].join("\n\n");

test("a row's words read the computed style of no element that cannot scroll", async () => {
  const sheet = wordsScrollSheet();
  document.head.append(sheet);
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const tree = displayTree(MANY_PLAIN_AND_ONE_PRE);
  const reads = spyOn(globalThis, "getComputedStyle");
  const view = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/issues/CORE-1"]}>
        <NoTooltips>
          <ReferenceNamesProvider>
            <div data-node-words="">
              <RowText markdown={MANY_PLAIN_AND_ONE_PRE} tree={tree} />
            </div>
          </ReferenceNamesProvider>
        </NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  try {
    const words = view.container.querySelector<HTMLElement>("[data-node-words]");
    if (words === null) throw new Error("the row draws no words' box");
    // The pass has run once the code block, which scrolls under the rules, is out of the tab
    // order: it read that block's style.
    const pre = await waitFor(() => {
      const found = words.querySelector<HTMLElement>("pre");
      expect(found, "the words draw no code block").not.toBeNull();
      expect(found?.getAttribute("tabindex"), "the code block is in the tab order").toBe("-1");
      return found as HTMLElement;
    });
    const plain = [...words.querySelectorAll("*")].filter(
      (element) => element !== pre && !pre.contains(element)
    );
    expect(
      plain.length,
      "the words draw too few plain elements to tell a read of each from none"
    ).toBeGreaterThan(100);
    const read = new Set(
      reads.mock.calls.flatMap(([element]) =>
        element instanceof Element && words.contains(element) ? [element] : []
      )
    );
    expect(
      [...read].map((element) => element.localName),
      `the row read the computed style of ${read.size} elements of its words, ${
        [...read].filter((element) => element !== pre).length
      } of them elements that cannot scroll`
    ).toEqual(["pre"]);
  } finally {
    reads.mockRestore();
    view.unmount();
    sheet.remove();
  }
});
