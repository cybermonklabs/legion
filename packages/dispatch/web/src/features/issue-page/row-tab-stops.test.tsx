import { beforeAll, expect, spyOn, test } from "bun:test";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, waitFor } from "@testing-library/react";
import { Fragment, type ReactNode, useEffect, useRef, useState } from "react";
import { MemoryRouter } from "react-router-dom";

import { displayTree } from "../../__tests__/display-trees";
import { installIntersectionObserver } from "../../__tests__/intersection-observer";
import { installResizeObserver } from "../../__tests__/scroll-box";
import { warmMarkdownRenderer } from "../refs/markdown-engine";
import { ReferenceNamesProvider } from "../refs/reference-names";
import { CutWords } from "./CutWords";
import { measureNearWords } from "./clamp-fixtures";
import type { NodeFact } from "./issue-tree";
import { NodeFacts } from "./NodeFacts";
import { NoTooltips } from "./NoTooltips";
import { CONTROL_LABELS } from "./page-words";
import { Publisher, publishedNames } from "./projection-fixture";
import { RowText, useOutOfTabOrder } from "./RowText";
import { overflowing, wordsScrollSheet } from "./words-scroll-fixtures";

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
  // On Element, which owns the method: a spy on HTMLElement leaves an own property behind once
  // restored, which hides every later spy on Element (Margin.landing.test.tsx's `stubRects`).
  const box = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
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

test("RowText keeps its links out of the tab order, and in it with tabStops, a late link included", async () => {
  const text = "Read [the notes](https://example.com/notes) before CORE-2.";
  // The names the page's one batched read brought, published as the page publishes them.
  const names = publishedNames(new Map([["dispatch://CORE-2", "Draw the children"]]));
  for (const tabStops of [false, true]) {
    const view = draw(
      <ReferenceNamesProvider>
        <Publisher published={names} />
        <RowText markdown={text} tabStops={tabStops} tree={displayTree(text)} />
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

/** The issues `PlainWords` links, one for each of its 25 steps. */
const PLAIN_STEPS = Array.from({ length: 25 }, (_unused, at) => `CORE-${at + 1}`);

/** A box `useOutOfTabOrder` keeps out of the tab order, with `keepScrollers` as given, holding a
 *  row's words of 200 elements - paragraphs, spans, links, strong and emphasised words, lists and
 *  their items, eight for each step of `PLAIN_STEPS` - none of which scrolls. */
function PlainWords({ keepScrollers }: { keepScrollers: boolean }): ReactNode {
  const box = useRef<HTMLDivElement>(null);
  useOutOfTabOrder(box, true, { keepScrollers });
  return (
    <div data-plain-words="" ref={box}>
      {PLAIN_STEPS.map((key) => (
        <Fragment key={key}>
          <p>
            <span>Step</span> <a href={`/issues/${key}`}>{key}</a> <strong>held</strong>{" "}
            <em>loose</em>
          </p>
          <ul>
            <li>one</li>
            <li>two</li>
          </ul>
        </Fragment>
      ))}
    </div>
  );
}

// A row's words hold hundreds of elements, and a style read for each of them on every pass cost
// seconds of a large issue's load. The pass reads the style only of an element that can scroll -
// a code block, a table, a diagram's preview, one whose own style sets its overflow - so words
// that hold none read no element's style, with `keepScrollers` and without, a late link's pass
// included, while every link still leaves the tab order.
test("a row of 200 elements that scroll nothing reads no element's style", async () => {
  const styles = spyOn(window, "getComputedStyle");
  const view = draw(<PlainWords keepScrollers={false} />);
  try {
    const box = view.container.querySelector<HTMLElement>("[data-plain-words]");
    if (box === null) throw new Error("the box draws no words");
    expect(box.querySelectorAll("*")).toHaveLength(200);
    view.redraw(<PlainWords keepScrollers />);
    const late = document.createElement("a");
    late.href = "/issues/CORE-99";
    act(() => {
      box.append(late);
    });
    await waitFor(() => {
      expect(outOfTabOrder(late)).toBe(true);
    });
    expect(
      [...box.querySelectorAll("a")].filter((link) => !outOfTabOrder(link)).length,
      "a link kept its tab stop"
    ).toBe(0);
    expect(
      styles.mock.calls.filter(([element]) => box.contains(element)).length,
      "the pass read the style of an element that scrolls nothing"
    ).toBe(0);
  } finally {
    view.unmount();
    styles.mockRestore();
  }
});

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
 *  `drawBlock` gives it) and a box of another kind, each able to scroll sideways, and each with
 *  somewhere to scroll to where the test lays its content out past its box (`overflowKept`). */
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
      <div data-other="" style={{ overflowX: "auto" }}>
        A line of output wider than its box
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

/** Lays the content of every scroller of `KeptScrollers` out past its box, sideways, as each
 *  scrolls, until the answer is called. */
function overflowKept(): () => void {
  return overflowing(KEPT_SCROLLERS.join(", "));
}

// Decision 317: hiding every scroller from the keyboard leaves a reader on the keyboard no way to
// scroll a wide code block or table sideways, so the focused row's scrollers keep their tab stop,
// each named by what it scrolls. A `pre` and a wrapper may carry a name only with a role.
test("useOutOfTabOrder with keepScrollers keeps a code block and a table's wrapper in the tab order, each named by what it scrolls", () => {
  const restore = overflowKept();
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, ["[data-code]", "[data-table]"])).toEqual([
      { label: CONTROL_LABELS.scrolls_code, role: "region", tabIndex: "0" },
      { label: CONTROL_LABELS.scrolls_table, role: "region", tabIndex: "0" },
    ]);
  } finally {
    view.unmount();
    restore();
  }
});

// Decision 317 covers every scroller of the focused row, so none stays out of reach: a drawn
// diagram's preview is named as a diagram, with a role, since a `div` takes a name only with one.
test("useOutOfTabOrder with keepScrollers keeps a drawn diagram's preview in the tab order, named as a diagram", () => {
  const restore = overflowKept();
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, ["[data-diagram]"])).toEqual([
      { label: CONTROL_LABELS.scrolls_diagram, role: "region", tabIndex: "0" },
    ]);
  } finally {
    view.unmount();
    restore();
  }
});

// A scroller of a kind the page has no word of its own for takes the word for content, in the
// form of the other names: "Content, scrolls sideways" (decision 317), and this one, like each of
// the others, scrolls sideways.
test("useOutOfTabOrder with keepScrollers keeps a scroller of any other kind in the tab order, named as content", () => {
  const restore = overflowKept();
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, ["[data-other]"])).toEqual([
      { label: CONTROL_LABELS.scrolls_content, role: "region", tabIndex: "0" },
    ]);
  } finally {
    view.unmount();
    restore();
  }
});

// A scroller whose content fits its box has nowhere to scroll, and Chromium gives it no tab stop,
// though `styles.css` lets every code block and table in a row's words scroll sideways, a one-line
// one included. So the focused row keeps only the scrollers whose content runs past their box, and
// one whose content comes to fit leaves the tab order at the next change inside the box, with its
// own name and role again.
test("useOutOfTabOrder with keepScrollers keeps only a scroller whose content runs past its box", async () => {
  let restore = overflowing("[data-code], [data-table]");
  const view = draw(<KeptScrollers keepScrollers />);
  try {
    expect(keptState(view.container, ["[data-code]", "[data-table]", "[data-diagram]"])).toEqual([
      { label: CONTROL_LABELS.scrolls_code, role: "region", tabIndex: "0" },
      { label: CONTROL_LABELS.scrolls_table, role: "region", tabIndex: "0" },
      { label: null, role: null, tabIndex: "-1" },
    ]);
    restore();
    restore = overflowing("[data-code]");
    act(() => {
      view.container.querySelector("[data-code]")?.append(" --verbose");
    });
    await waitFor(() => {
      expect(keptState(view.container, ["[data-code]", "[data-table]"])).toEqual([
        { label: CONTROL_LABELS.scrolls_code, role: "region", tabIndex: "0" },
        { label: null, role: null, tabIndex: "-1" },
      ]);
    });
    expect(view.container.querySelectorAll("[data-kept-scroller]")).toHaveLength(1);
  } finally {
    view.unmount();
    restore();
  }
});

// Decision 317: a scroller counts as one that scrolls whenever its content runs past its box,
// judged again on every change of its width, so a block that starts to overflow when the window
// narrows gains its tab stop, and one that stops overflowing when it widens loses it, with its own
// name and role again. A width changes no child of the box, so only a size observer sees it: of
// the box, which narrows with the window, and of the scroller itself. The observer stops with the
// box.
test("useOutOfTabOrder with keepScrollers judges a scroller again on every change of its width", () => {
  const sizes = installResizeObserver();
  let restore = () => {};
  try {
    const view = draw(<KeptScrollers keepScrollers />);
    try {
      const box = view.container.firstElementChild;
      const code = view.container.querySelector("[data-code]");
      if (box === null || code === null) throw new Error("the box draws no code block");
      expect(keptState(view.container, ["[data-code]"])).toEqual([
        { label: null, role: null, tabIndex: "-1" },
      ]);
      // The window narrows the box, and the code block's line runs past its own box.
      restore = overflowing("[data-code]");
      sizes.resize(box);
      expect(
        keptState(view.container, ["[data-code]"]),
        "a code block that came to overflow as the box narrowed has no tab stop"
      ).toEqual([{ label: CONTROL_LABELS.scrolls_code, role: "region", tabIndex: "0" }]);
      // The code block widens again, and its line fits.
      restore();
      restore = () => {};
      sizes.resize(code);
      expect(
        keptState(view.container, ["[data-code]"]),
        "a code block that came to fit as its width changed keeps its tab stop"
      ).toEqual([{ label: null, role: null, tabIndex: "-1" }]);
    } finally {
      view.unmount();
    }
    expect(sizes.live(), "the size observer outlived the box").toBe(0);
  } finally {
    restore();
    sizes.restore();
  }
});

// The size observer of decision 317 watches the box and each of its scrollers once: a browser
// reports a target again each time it is watched, and each report runs the pass that watches
// them, so a second watch would report the scrollers on every frame. A scroller that leaves the
// box is let go, so the observer holds nothing the row no longer draws.
test("useOutOfTabOrder with keepScrollers watches each scroller once, and lets go of one that leaves the box", async () => {
  const sizes = installResizeObserver();
  const unobserve = spyOn(ResizeObserver.prototype, "unobserve");
  try {
    const view = draw(<KeptScrollers keepScrollers />);
    try {
      const box = view.container.firstElementChild;
      const other = view.container.querySelector("[data-other]");
      if (box === null || other === null) throw new Error("the box draws no other scroller");
      // A target by the mark it carries, so a failure prints words: printed whole, an element of
      // the test's DOM drags its document with it.
      const markOf = (target: Element) =>
        target === box ? "box" : (KEPT_SCROLLERS.find((mark) => target.matches(mark)) ?? "?");
      // A second pass: the window narrows the box.
      sizes.resize(box);
      expect(sizes.observed().map(markOf), "a second pass watched a scroller again").toEqual([
        "box",
        ...KEPT_SCROLLERS,
      ]);
      act(() => {
        other.remove();
      });
      await waitFor(() => {
        expect(
          unobserve.mock.calls.map(([target]) => markOf(target)),
          "the scroller that left the box is still watched"
        ).toEqual(["[data-other]"]);
      });
      sizes.resize(box);
      expect(
        unobserve.mock.calls.map(([target]) => markOf(target)),
        "a scroller was let go twice, or one in the box was"
      ).toEqual(["[data-other]"]);
    } finally {
      view.unmount();
    }
  } finally {
    unobserve.mockRestore();
    sizes.restore();
  }
});

// A row that loses the focus stops passing `keepScrollers`, and its scrollers leave the tab order
// again, with no name or role left of the ones they carried while it held the focus.
test("useOutOfTabOrder takes every kept scroller out of the tab order, unnamed, once keepScrollers turns off", () => {
  const restore = overflowKept();
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
    restore();
  }
});

// A tree's keys tell a kept scroller from the row's other controls by its mark, so each scroller
// carries `data-kept-scroller` while it keeps its tab stop, no other element does, and none keeps
// it once the row stops passing `keepScrollers`.
test("useOutOfTabOrder marks each kept scroller with data-kept-scroller, and no scroller once keepScrollers turns off", () => {
  const restore = overflowKept();
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
    restore();
  }
});
