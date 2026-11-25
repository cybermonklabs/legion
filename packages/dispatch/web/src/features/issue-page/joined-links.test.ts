import { beforeAll, expect, test } from "bun:test";
import { waitFor } from "@testing-library/react";
import { createElement } from "react";

import { displayTree } from "../../__tests__/display-trees";
import { renderWithQueries as render } from "../../__tests__/query-provider";
import { MarkdownBody } from "../refs/MarkdownBody";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { joinSplitLinks } from "./joined-links";

/**
 * A link whose words carry a mark draws as one anchor on the new page (decision 407). The display
 * schema ranks `link` innermost, so the serializer closes the anchor around a marked run and opens
 * it again after: three anchors for one link, three stops of the `f` key, three links read aloud.
 * `joinSplitLinks` joins them after each paint, as the row's painter does (`RowText.tsx`).
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** The mark of the element the case paints into, which each paint's pass joins inside. */
const PAINTED = "[data-painted]";

/** `markdown` painted from the server's reading of it, as a row draws a stored text, with each
 *  paint's split links joined, as the row's own pass joins them. */
async function painted(markdown: string): Promise<{ body: HTMLElement; unmount: () => void }> {
  const view = render(
    createElement(
      "div",
      { "data-painted": "" },
      createElement(MarkdownBody, {
        markdown,
        onRendered: () => {
          const element = document.querySelector(PAINTED);
          if (element !== null) joinSplitLinks(element);
        },
        referenceText: "",
        tree: displayTree(markdown),
      })
    )
  );
  const body = await waitFor(() => {
    const element = view.container.querySelector<HTMLElement>(PAINTED);
    expect(element?.textContent, "the text was never painted").toBeTruthy();
    return element as HTMLElement;
  });
  return { body, unmount: () => view.unmount() };
}

test("a link whose words carry a mark is one anchor", async () => {
  const { body, unmount } = await painted("[the **HTTP** rule](https://example.com/x)");
  try {
    const anchors = [...body.querySelectorAll("a")];
    expect(
      anchors.map((anchor) => anchor.outerHTML),
      "the marked link was drawn as more than one anchor"
    ).toHaveLength(1);
    const [anchor] = anchors;
    expect(anchor).toHaveAttribute("href", "https://example.com/x");
    expect(anchor?.textContent).toBe("the HTTP rule");
    expect(anchor?.querySelector("strong")?.textContent, "the anchor lost its mark").toBe("HTTP");
  } finally {
    unmount();
  }
});

test("two links to the same address split by plain words stay two", async () => {
  const { body, unmount } = await painted(
    "[one](https://example.com/x) and [two](https://example.com/x)"
  );
  try {
    expect([...body.querySelectorAll("a")].map((anchor) => anchor.textContent)).toEqual([
      "one",
      "two",
    ]);
  } finally {
    unmount();
  }
});
