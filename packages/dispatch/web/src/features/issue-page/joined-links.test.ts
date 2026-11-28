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

// A mark that holds more than one run of the link's words, such as bold words around a code span,
// draws anchors inside the mark and anchors around it: the anchors inside the mark join into one,
// and that one joins the anchors around the mark, so the link is still one stop and one link.
test("a link whose marked words hold a second mark is one anchor", async () => {
  const { body, unmount } = await painted("[s **t `u`** v](https://example.com/v)");
  try {
    const anchors = [...body.querySelectorAll("a")];
    expect(
      anchors.map((anchor) => anchor.outerHTML),
      "the link whose marked words hold a second mark was drawn as more than one anchor"
    ).toHaveLength(1);
    const [anchor] = anchors;
    expect(anchor).toHaveAttribute("href", "https://example.com/v");
    expect(anchor?.textContent).toBe("s t u v");
    expect(anchor?.querySelector("strong code")?.textContent, "the anchor lost its marks").toBe(
      "u"
    );
  } finally {
    unmount();
  }
});

// A link that opens with such a mark has no anchor before the mark: once the anchors inside the
// mark join, the mark holds one anchor, which a later pass joins to the anchors after it.
test("a link that opens with marked words holding a second mark is one anchor", async () => {
  const { body, unmount } = await painted("[**p `q`** r](https://example.com/w)");
  try {
    const anchors = [...body.querySelectorAll("a")];
    expect(
      anchors.map((anchor) => anchor.outerHTML),
      "the link that opens with marked words holding a second mark was drawn as more than one anchor"
    ).toHaveLength(1);
    const [anchor] = anchors;
    expect(anchor).toHaveAttribute("href", "https://example.com/w");
    expect(anchor?.textContent).toBe("p q r");
    expect(anchor?.querySelector("strong code")?.textContent, "the anchor lost its marks").toBe(
      "q"
    );
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

/** `html` as the painter draws it, in an element of its own, with its split links joined. */
function joined(html: string): HTMLElement {
  const body = document.createElement("div");
  body.innerHTML = html;
  joinSplitLinks(body);
  return body;
}

// A long text is painted in slices, and the row's pass joins after each, so it can meet a body the
// reference pass has not reached: each `dispatch://` link is still Proof's `href=""`, its address
// in `data-dispatch-href`. Two references side by side are two links all the same.
test("two references to different targets side by side stay two links", () => {
  const body = joined(
    '<p><a href="" data-dispatch-href="dispatch://CORE-1/spec">one</a>' +
      '<strong><a href="" data-dispatch-href="dispatch://CORE-2">two</a></strong></p>'
  );
  expect(
    [...body.querySelectorAll("a")].map((anchor) => [
      anchor.getAttribute("data-dispatch-href"),
      anchor.textContent,
    ]),
    `the two references were drawn as ${body.innerHTML}`
  ).toEqual([
    ["dispatch://CORE-1/spec", "one"],
    ["dispatch://CORE-2", "two"],
  ]);
});

// ProseMirror draws two link marks side by side as one anchor only where their attributes are
// equal: two links to one address with different titles are two.
test("two pieces of a link with different titles stay two anchors", async () => {
  const { body, unmount } = await painted(
    '[a](https://example.com/u "t1")**[b](https://example.com/u "t2")**'
  );
  try {
    expect(
      [...body.querySelectorAll("a")].map((anchor) => [
        anchor.getAttribute("title"),
        anchor.textContent,
      ]),
      `the two links were drawn as ${body.innerHTML}`
    ).toEqual([
      ["t1", "a"],
      ["t2", "b"],
    ]);
  } finally {
    unmount();
  }
});

// A dashboard address of this origin is one the reference pass rewrites later, and the portal it
// hands each anchor draws that anchor's words: a join before that pass would take two of them off
// the page.
test("a dashboard link painted before the reference pass stays three anchors", () => {
  const address = `${window.location.origin}/issues/CORE-1/spec`;
  const body = joined(
    `<p><a href="${address}">the </a><strong><a href="${address}">spec</a></strong>` +
      `<a href="${address}"> rule</a></p>`
  );
  expect(
    [...body.querySelectorAll("a")].map((anchor) => anchor.textContent),
    `the dashboard link was drawn as ${body.innerHTML}`
  ).toEqual(["the ", "spec", " rule"]);
});
