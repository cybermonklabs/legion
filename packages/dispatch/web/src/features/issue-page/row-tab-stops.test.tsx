import { beforeAll, expect, spyOn, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { type ReactNode, useEffect, useMemo, useRef, useState } from "react";
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
import { RowText, useOutOfTabOrder } from "./RowText";
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

/** A box `useOutOfTabOrder` keeps out of the tab order, holding no control: a wide block that
 *  scrolls across, a tall one that scrolls down, and one whose content overflows it unscrolled. */
function Scrollers(): ReactNode {
  const box = useRef<HTMLDivElement>(null);
  useOutOfTabOrder(box, true);
  return (
    <div ref={box}>
      <pre data-wide="" style={{ overflowX: "auto" }}>
        bun run e2e --project=chromium
      </pre>
      <div data-tall="" style={{ overflowY: "scroll" }}>
        Twelve lines of output
      </div>
      <div data-visible="" style={{ overflow: "visible" }}>
        Plain words
      </div>
    </div>
  );
}

// In Chromium an element that scrolls its own content and holds no control in the tab order is
// a tab stop of its own, so a row's wide code block or table wrapper would be a second stop
// beside the outline's. The box takes each such element out of the tab order, a late one
// included, and leaves an element that does not scroll as it was.
test("useOutOfTabOrder takes an element that scrolls out of the tab order, a late one included, and leaves one that does not", async () => {
  const view = draw(<Scrollers />);
  try {
    const wrapper = document.createElement("div");
    wrapper.style.overflowX = "auto";
    act(() => {
      view.container.querySelector("[data-visible]")?.after(wrapper);
    });
    await waitFor(() => {
      expect(
        [
          view.container.querySelector("[data-wide]"),
          view.container.querySelector("[data-tall]"),
          wrapper,
        ].map(outOfTabOrder)
      ).toEqual([true, true, true]);
    });
    expect(view.container.querySelector("[data-visible]")?.hasAttribute("tabindex")).toBe(false);
  } finally {
    view.unmount();
  }
});

/** The rules of `styles.css` that give a block in a node's words a scroll of its own (decision
 *  282), as one style sheet: no DOM of a unit test loads the page's styles, and the overflow
 *  `useOutOfTabOrder` reads is the one these rules compute. */
function wordsScrollSheet(): HTMLStyleElement {
  const css = readFileSync(join(import.meta.dir, "..", "..", "styles.css"), "utf8").replace(
    /\/\*[\s\S]*?\*\//g,
    ""
  );
  const rules = [...css.matchAll(/([^{};]+)\{([^{}]*)\}/g)].flatMap(
    ([rule, selectors = "", body = ""]) =>
      selectors.includes("[data-node-words]") && /overflow-x:\s*auto/.test(body)
        ? [rule.trim()]
        : []
  );
  expect(rules.length, "no rule of styles.css scrolls a block in a node's words").toBeGreaterThan(
    0
  );
  const sheet = document.createElement("style");
  sheet.textContent = rules.join("\n");
  return sheet;
}

/** A comment that holds a code block and a table, each wider than a row's words. */
const WIDE_BLOCKS = [
  "Run it so:",
  "",
  "```sh",
  `bun run e2e ${Array.from({ length: 40 }, (_unused, at) => `--flag${at + 1}`).join(" ")}`,
  "```",
  "",
  `| ${Array.from({ length: 16 }, (_unused, at) => `column${at + 1}`).join(" | ")} |`,
  `| ${Array.from({ length: 16 }, () => "---").join(" | ")} |`,
  `| ${Array.from({ length: 16 }, (_unused, at) => `value${at + 1}`).join(" | ")} |`,
].join("\n");

/** Draws `WIDE_BLOCKS` as a row draws a node's words, under the rules that make its code block
 *  and its table scroll, and answers the two once both are drawn. */
async function drawWideBlocks(tabStops: boolean) {
  const sheet = wordsScrollSheet();
  document.head.append(sheet);
  const view = draw(
    <ReferenceNamesProvider>
      <div data-node-words="">
        <RowText markdown={WIDE_BLOCKS} tabStops={tabStops} tree={displayTree(WIDE_BLOCKS)} />
      </div>
    </ReferenceNamesProvider>
  );
  const close = () => {
    view.unmount();
    sheet.remove();
  };
  try {
    const blocks = await waitFor(() => {
      const pre = view.container.querySelector<HTMLElement>("[data-node-words] pre");
      const table = view.container.querySelector<HTMLElement>("[data-node-words] table");
      if (pre === null || table === null) throw new Error("the words draw no code block or table");
      return { pre, table };
    });
    // The rules apply, so each block scrolls as the browser draws it.
    expect([blocks.pre, blocks.table].map((block) => getComputedStyle(block).overflowX)).toEqual([
      "auto",
      "auto",
    ]);
    return { ...blocks, close };
  } catch (error) {
    close();
    throw error;
  }
}

// The outline is one tab stop (R9), and in Chromium a block that scrolls and holds no control in
// the tab order is a tab stop of its own: a node's words drawn out of the tab order take their
// wide code block and table out of it, as styles.css makes each scroll.
test("RowText out of the tab order takes its wide code block and table out of it", async () => {
  const { close, pre, table } = await drawWideBlocks(false);
  try {
    await waitFor(() => {
      expect([pre, table].map(outOfTabOrder)).toEqual([true, true]);
    });
  } finally {
    close();
  }
});

// A view with tab stops has no tree's keys, so a wide block keeps the tab stop Chromium gives it:
// it is how a keyboard reaches its scroll.
test("RowText with tabStops leaves its wide code block and table in the tab order, for the keyboard's scroll", async () => {
  const { close, pre, table } = await drawWideBlocks(true);
  try {
    expect([pre, table].map((block) => block.hasAttribute("tabindex"))).toEqual([false, false]);
  } finally {
    close();
  }
});

/** A box `useOutOfTabOrder` keeps out of the tab order, with `keepScrollers` as given: a code
 *  block, a table's wrapper, a drawn diagram's preview (the class `mermaid-diagrams.ts`'s
 *  `drawBlock` gives it) and a box of another kind, each scrolling. */
function KeptScrollers({ keepScrollers }: { keepScrollers: boolean }): ReactNode {
  const box = useRef<HTMLDivElement>(null);
  useOutOfTabOrder(box, true, { keepScrollers });
  return (
    <div ref={box}>
      <pre data-code="" style={{ overflowX: "auto" }}>
        bun run e2e --project=chromium
      </pre>
      <div data-table="" style={{ overflowX: "auto" }}>
        <table>
          <tbody>
            <tr>
              <td>value1</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div className="dispatch-mermaid-preview" data-diagram="" style={{ overflowX: "auto" }} />
      <div data-other="" style={{ overflowY: "scroll" }}>
        Twelve lines of output
      </div>
    </div>
  );
}

/** What `keptState` reads of one scroller of the box. */
interface ScrollerState {
  readonly label: string | null;
  readonly role: string | null;
  readonly tabIndex: string | null;
}

/** Every scroller of `KeptScrollers`, by the mark it carries. */
const KEPT_SCROLLERS = ["[data-code]", "[data-table]", "[data-diagram]", "[data-other]"];

/** The tab index, name and role of each scroller `selectors` names, as drawn. */
function keptState(container: HTMLElement, selectors: readonly string[]): ScrollerState[] {
  return selectors.map((selector) => {
    const scroller = container.querySelector(selector);
    return {
      label: scroller?.getAttribute("aria-label") ?? null,
      role: scroller?.getAttribute("role") ?? null,
      tabIndex: scroller?.getAttribute("tabindex") ?? null,
    };
  });
}

// Decision 317: hiding every scroller from the keyboard leaves a reader on the keyboard no way to
// scroll a wide code block or table sideways, so the focused row's scrollers keep their tab stop,
// each named by what it scrolls. A `pre` and a wrapper may carry a name only with a role.
test("useOutOfTabOrder with keepScrollers keeps a code block and a table's wrapper in the tab order, each named by what it scrolls", () => {
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, ["[data-code]", "[data-table]"])).toEqual([
      { label: CONTROL_LABELS.scrolls_code, role: "region", tabIndex: "0" },
      { label: CONTROL_LABELS.scrolls_table, role: "region", tabIndex: "0" },
    ]);
  } finally {
    view.unmount();
  }
});

// Decision 317 covers every scroller of the focused row, so none stays out of reach: a drawn
// diagram's preview is named as a diagram, with a role, since a `div` takes a name only with one.
test("useOutOfTabOrder with keepScrollers keeps a drawn diagram's preview in the tab order, named as a diagram", () => {
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, ["[data-diagram]"])).toEqual([
      { label: CONTROL_LABELS.scrolls_diagram, role: "region", tabIndex: "0" },
    ]);
  } finally {
    view.unmount();
  }
});

// A scroller of a kind the page has no word of its own for takes the word for content.
test("useOutOfTabOrder with keepScrollers keeps a scroller of any other kind in the tab order, named as content", () => {
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, ["[data-other]"])).toEqual([
      { label: CONTROL_LABELS.scrolls_content, role: "region", tabIndex: "0" },
    ]);
  } finally {
    view.unmount();
  }
});

// A row that loses the focus stops passing `keepScrollers`, and its scrollers leave the tab order
// again, with no name or role left of the ones they carried while it held the focus.
test("useOutOfTabOrder takes every kept scroller out of the tab order, unnamed, once keepScrollers turns off", () => {
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, KEPT_SCROLLERS).map((state) => state.tabIndex)).toEqual([
      "0",
      "0",
      "0",
      "0",
    ]);
    view.redraw(<KeptScrollers keepScrollers={false} />);
    expect(keptState(view.container, KEPT_SCROLLERS)).toEqual(
      KEPT_SCROLLERS.map(() => ({ label: null, role: null, tabIndex: "-1" }))
    );
  } finally {
    view.unmount();
  }
});

// A tree's keys tell a kept scroller from the row's other controls by its mark, so each scroller
// carries `data-kept-scroller` while it keeps its tab stop, no other element does, and none keeps
// it once the row stops passing `keepScrollers`.
test("useOutOfTabOrder marks each kept scroller with data-kept-scroller, and no scroller once keepScrollers turns off", () => {
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(
      KEPT_SCROLLERS.map((selector) =>
        view.container.querySelector(selector)?.getAttribute("data-kept-scroller")
      )
    ).toEqual(["", "", "", ""]);
    expect(view.container.querySelectorAll("[data-kept-scroller]")).toHaveLength(4);
    view.redraw(<KeptScrollers keepScrollers={false} />);
    expect(view.container.querySelectorAll("[data-kept-scroller]")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});
