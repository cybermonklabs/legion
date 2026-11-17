import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { type ReactNode, useEffect, useMemo, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { CutWords } from "./CutWords";
import { measureNearWords } from "./clamp-fixtures";
import type { NodeFact } from "./issue-tree";
import { NodeFacts } from "./NodeFacts";
import { NoTooltips } from "./NoTooltips";
import { CONTROL_LABELS } from "./page-words";
import { RowText } from "./RowText";
import { ReferenceNamesProvider, usePublishReferenceNames } from "./reference-names";

/**
 * Whether the controls inside a node's words and facts take a place in the tab order (part G's
 * R9). The outline is one tab stop and its keys reach a row's
 * controls, so `CutWords`, `NodeFacts` and `RowText` keep every control out of the tab order by
 * default, the ones that arrive after the first paint included; a view that no tree's keys reach
 * passes `tabStops`, and every control keeps its place, a late one included.
 */

beforeAll(async () => {
  await warmMarkdownRenderer();
});

/** Whether `control` is out of the tab order: the page set it to -1. */
function outOfTabOrder(control: Element | null): boolean {
  return control?.getAttribute("tabindex") === "-1";
}

/** Draws `children` as the page's slots draw a row's parts, inside `NoTooltips`; `redraw` draws
 *  them again in the same tree, as a read that answers later does. */
function draw(children: ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Number.POSITIVE_INFINITY } },
  });
  const page = (inside: ReactNode) => (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <NoTooltips>{inside}</NoTooltips>
      </MemoryRouter>
    </QueryClientProvider>
  );
  const view = render(page(children));
  return { ...view, redraw: (inside: ReactNode) => view.rerender(page(inside)) };
}

/** A link drawn after the first paint, as a reading's link arrives once its target answers. */
function LateLink(): ReactNode {
  const [shown, setShown] = useState(false);
  useEffect(() => {
    const timer = setTimeout(() => setShown(true), 0);
    return () => clearTimeout(timer);
  }, []);
  return shown ? (
    <a data-late="" href="/issues/CORE-3">
      CORE-3
    </a>
  ) : null;
}

test("CutWords keeps its words' controls out of the tab order, and in it with tabStops, a late link included", async () => {
  for (const tabStops of [false, true]) {
    const view = draw(
      <CutWords className="" tabStops={tabStops} whole={false}>
        <p>
          <a data-early="" href="/issues/CORE-2">
            CORE-2
          </a>{" "}
          <LateLink />
        </p>
      </CutWords>
    );
    try {
      const late = await waitFor(() => {
        const found = view.container.querySelector("[data-late]");
        expect(found).not.toBeNull();
        return found;
      });
      await waitFor(() => {
        expect(outOfTabOrder(view.container.querySelector("[data-early]"))).toBe(!tabStops);
        expect(outOfTabOrder(late)).toBe(!tabStops);
      });
    } finally {
      view.unmount();
    }
  }
});

/** Lays the words out as a cut statement's are, until the answer is called: every element
 *  600 px tall from the top, save the one `below` names, which stands 400 px down, under the
 *  six lines the cut draws. No DOM of a unit test lays anything out, and the cut reads the
 *  words' own boxes (`clamp-measurer.ts`'s `measureClamp`). */
function layOutCut(below: string): () => void {
  const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollHeight");
  Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
    configurable: true,
    get: () => 600,
  });
  const box = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this.matches(below) ? new DOMRect(0, 400, 60, 24) : new DOMRect(0, 0, 600, 600);
  });
  return () => {
    if (own === undefined) Reflect.deleteProperty(HTMLElement.prototype, "scrollHeight");
    else Object.defineProperty(HTMLElement.prototype, "scrollHeight", own);
    box.mockRestore();
  };
}

// Decision 85: in a view with tab stops, a link the cut hides leaves the tab order while the
// words are cut, so Tab reaches "more" and puts the focus nowhere the reader cannot see; a link
// above the cut keeps its place, and once the words are whole the hidden one takes its own back.
test("a cut statement with a hidden link gives that link no tab stop while cut, and a tab stop once whole", async () => {
  const restore = layOutCut("[data-hidden]");
  const near = installIntersectionObserver();
  const words = Array.from({ length: 100 }, (_unused, at) => `word${at + 1}`).join(" ");
  const view = draw(
    <CutWords className="" tabStops whole={false}>
      <p>
        <a data-shown="" href="/issues/CORE-2">
          CORE-2
        </a>{" "}
        {words}{" "}
        <a data-hidden="" href="/issues/CORE-3">
          CORE-3
        </a>
      </p>
    </CutWords>
  );
  try {
    // The page measures only the words of rows near the viewport.
    await measureNearWords(near);
    const more = await waitFor(() => {
      const found = view.container.querySelector<HTMLButtonElement>("[data-node-more]");
      expect(found, "the statement is not cut").not.toBeNull();
      return found;
    });
    const hidden = view.container.querySelector("[data-hidden]");
    const shown = view.container.querySelector("[data-shown]");
    expect(outOfTabOrder(hidden), "the link the cut hides keeps a tab stop").toBe(true);
    expect(shown?.hasAttribute("tabindex"), "the link above the cut lost its tab stop").toBe(false);
    act(() => {
      more?.click();
    });
    await waitFor(() => {
      expect(view.container.querySelector("[data-node-more]")?.textContent).toBe(
        CONTROL_LABELS.read_less
      );
    });
    expect(
      hidden?.hasAttribute("tabindex"),
      "the link the cut hid has no tab stop once the words are whole"
    ).toBe(false);
    expect(shown?.hasAttribute("tabindex")).toBe(false);
  } finally {
    view.unmount();
    near.restore();
    restore();
  }
});

const COMMAND: NodeFact = { draw: "command", name: "run", value: "bun run e2e" };
const LATER: NodeFact = { draw: "command", name: "run", value: "bun run test:web" };

test("NodeFacts keeps its copy controls out of the tab order, and in it with tabStops, a late one included", async () => {
  for (const tabStops of [false, true]) {
    const view = draw(<NodeFacts facts={[COMMAND]} tabStops={tabStops} />);
    try {
      // A fact the read brings after the first paint draws its own copy control then.
      view.redraw(<NodeFacts facts={[COMMAND, LATER]} tabStops={tabStops} />);
      await waitFor(() => {
        const controls = [...view.container.querySelectorAll("button")];
        expect(controls).toHaveLength(2);
        expect(controls.map(outOfTabOrder)).toEqual([!tabStops, !tabStops]);
      });
      // A picture fact draws no control (`DispatchPicture` with no anchor draws words or an
      // image), so no fact puts a control into the list after the first paint without a tab
      // index from a prop. The test puts a link into the list itself: only the list's watch
      // (`useOutOfTabOrder`) takes it out of the tab order.
      const late = document.createElement("a");
      late.href = "/issues/CORE-3";
      act(() => {
        view.container.querySelector("dl")?.append(late);
      });
      await waitFor(() => {
        expect(outOfTabOrder(late)).toBe(!tabStops);
        expect(late.hasAttribute("tabindex")).toBe(!tabStops);
      });
    } finally {
      view.unmount();
    }
  }
});

/** The names the page's one batched read brought, published as the page publishes them. */
function Named({ children }: { children: ReactNode }): ReactNode {
  const names = useMemo(() => new Map([["dispatch://CORE-2", "Draw the children"]]), []);
  usePublishReferenceNames(names);
  return children;
}

test("RowText keeps its links out of the tab order, and in it with tabStops, a late link included", async () => {
  const text = "Read [the notes](https://example.com/notes) before CORE-2.";
  for (const tabStops of [false, true]) {
    const view = draw(
      <ReferenceNamesProvider>
        <Named>
          <RowText markdown={text} tabStops={tabStops} tree={displayTree(text)} />
        </Named>
      </ReferenceNamesProvider>
    );
    try {
      // The reading's own link is drawn with the text; the key's link is the row's own pass,
      // made once the names have arrived.
      await waitFor(() => {
        const drawn = view.container.querySelector('a[href="https://example.com/notes"]');
        const late = view.container.querySelector('a[data-row-reference="dispatch://CORE-2"]');
        expect(drawn).not.toBeNull();
        expect(late).not.toBeNull();
        expect(outOfTabOrder(drawn)).toBe(!tabStops);
        expect(outOfTabOrder(late)).toBe(!tabStops);
      });
    } finally {
      view.unmount();
    }
  }
});
