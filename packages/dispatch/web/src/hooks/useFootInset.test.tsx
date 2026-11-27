import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { type RenderResult, render, screen } from "@testing-library/react";
import { type ReactNode, useRef } from "react";

import { nextFrame } from "../__tests__/frames";
import { DOCUMENT_SCROLL_ATTRIBUTE, MARK_ATTRIBUTE, useFootInset } from "./useFootInset";

// happy-dom lays nothing out and has no hit test, so the harness is the geometry. The float, in
// the foot's status layer as Pending changes is, stands 1rem above the foot of the screen, and so
// does the tall float beside it; every point of either hits the float and then the element a row
// names (`under`). Each row gives the boxes it needs (`boxes`), by `data-testid`, each a top and a
// bottom edge on the screen, and sizes each scroll container's padding box and content (`sized`).
// Every other element has no box. happy-dom computes no initial value for `top` and `bottom`, so
// each sticky element states its own, as a browser's computed style reads them.
/** The height of each float, by its testid. */
const FLOAT_HEIGHTS: Readonly<Record<string, number>> = { float: 48, "tall-float": 96 };
const REM = 16;

/** The top and bottom edge, on the screen, of each element that has a box, by its testid. */
let boxes: Record<string, { readonly top: number; readonly bottom: number }> = {};

/** The testid of the element under every point of the float. */
let under = "";

/** How many points the hook has asked the hit test about. */
let points = 0;

let restoreRects: () => void = () => undefined;

function rect(top: number, bottom: number, left: number, right: number): DOMRect {
  return {
    bottom,
    height: bottom - top,
    left,
    right,
    toJSON: () => ({}),
    top,
    width: right - left,
    x: left,
    y: top,
  };
}

beforeEach(() => {
  boxes = {};
  under = "";
  points = 0;
  const spy = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    const name = this.getAttribute("data-testid") ?? "";
    const floatHeight = FLOAT_HEIGHTS[name];
    if (floatHeight !== undefined) {
      return rect(window.innerHeight - REM - floatHeight, window.innerHeight - REM, 600, 900);
    }
    const placed = boxes[name];
    return placed === undefined ? rect(0, 0, 0, 0) : rect(placed.top, placed.bottom, 0, 1000);
  });
  restoreRects = () => spy.mockRestore();
  Object.defineProperty(document, "elementsFromPoint", {
    configurable: true,
    value: () => {
      points += 1;
      return [screen.getByTestId("float"), screen.getByTestId(under)];
    },
  });
});

afterEach(() => {
  restoreRects();
  Reflect.deleteProperty(document, "elementsFromPoint");
});

/** A ref that gives a scroll container a padding box `client` px tall around `scroll` px of
 *  content and padding. */
function sized(client: number, scroll: number) {
  return (element: HTMLElement | null) => {
    if (element === null) return;
    Object.defineProperties(element, {
      clientHeight: { configurable: true, get: () => client },
      scrollHeight: { configurable: true, get: () => scroll },
    });
  };
}

/** A float of the status layer while `active`, `float` or, by `testid`, `tall-float`. */
function Float({ active = true, testid = "float" }: { active?: boolean; testid?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useFootInset(ref, active, "status");
  return <div data-testid={testid} ref={ref} style={{ position: "fixed" }} />;
}

/** Renders `page` under the float, lets one pass run, and then runs `check`. */
async function afterAPass(page: ReactNode, check: () => void): Promise<void> {
  const view = render(
    <>
      {page}
      <Float />
    </>
  );
  try {
    await nextFrame();
    expect(points).toBeGreaterThan(0);
    check();
  } finally {
    view.unmount();
  }
}

/** The issues table's box, reaching the foot of the screen under the float and holding more rows
 *  than it shows, with its last row's first cell pinned on the left and stuck at `top`. */
function Table({ top }: { top: number | "auto" }) {
  return (
    <div data-testid="table-box" ref={sized(600, 2000)} style={{ overflowY: "auto" }}>
      <table>
        <tbody>
          <tr>
            <td data-testid="pinned" style={{ bottom: "auto", left: 0, position: "sticky", top }}>
              CORE-2
            </td>
            <td>Ship the parser</td>
          </tr>
        </tbody>
      </table>
    </div>
  );
}

test("a float over a cell sticky only on the left, in the table's last row, gives the table's box the end room", async () => {
  boxes = { "table-box": { bottom: window.innerHeight, top: 100 } };
  under = "pinned";
  await afterAPass(<Table top="auto" />, () => {
    expect(screen.getByTestId("table-box")).toHaveAttribute(MARK_ATTRIBUTE);
  });
});

test("a float over a cell stuck at the top gives no scroller further out the end room", async () => {
  boxes = { "table-box": { bottom: window.innerHeight, top: 100 } };
  under = "pinned";
  await afterAPass(<Table top={0} />, () => {
    expect(screen.getByTestId("table-box")).not.toHaveAttribute(MARK_ATTRIBUTE);
  });
});

test("a float over a column stuck at the height of the screen, whose short content does not overflow it, gives that column the end room", async () => {
  const height = window.innerHeight;
  boxes = { cards: { bottom: 300, top: 0 }, margin: { bottom: height, top: 0 } };
  under = "margin";
  await afterAPass(
    <>
      <main data-testid="main" id="main-content" {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }} />
      <aside
        data-testid="margin"
        ref={sized(height, height)}
        style={{ bottom: "auto", overflowY: "auto", position: "sticky", top: 0 }}
      >
        <div data-testid="cards">Keep alpha?</div>
      </aside>
    </>,
    () => {
      expect(screen.getByTestId("margin")).toHaveAttribute(MARK_ATTRIBUTE);
      expect(screen.getByTestId("main")).not.toHaveAttribute(MARK_ATTRIBUTE);
      expect(document.body).not.toHaveAttribute(MARK_ATTRIBUTE);
    }
  );
});

test("a float over a code block as tall as its content, whose last line ends 3px above its foot, gives the end room to main, which the document scrolls, and not to the code block", async () => {
  const height = window.innerHeight;
  boxes = {
    code: { bottom: height, top: height - 60 },
    line: { bottom: height - 3, top: height - 17 },
  };
  under = "line";
  await afterAPass(
    <main data-testid="main" id="main-content" {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }}>
      <pre data-testid="code" ref={sized(60, 60)} style={{ overflowX: "auto", overflowY: "auto" }}>
        <code data-testid="line" style={{ display: "inline" }}>
          let parsed = parse(source);
        </code>
      </pre>
    </main>,
    () => {
      expect(screen.getByTestId("code")).not.toHaveAttribute(MARK_ATTRIBUTE);
      expect(screen.getByTestId("main")).toHaveAttribute(MARK_ATTRIBUTE, "padding");
    }
  );
});

test("a float over an element outside main and inside no scroller gives no element the end room, body included", async () => {
  boxes = { beside: { bottom: window.innerHeight, top: 0 } };
  under = "beside";
  await afterAPass(
    <>
      <main data-testid="main" id="main-content" {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }} />
      <div data-testid="beside">Beside the page</div>
    </>,
    () => {
      expect(screen.getByTestId("main")).not.toHaveAttribute(MARK_ATTRIBUTE);
      expect(document.body).not.toHaveAttribute(MARK_ATTRIBUTE);
    }
  );
});

// The new issue page's frame states that the document scrolls its column, the tallest child of
// the row that holds the stuck Contents pane and the column, and not `main`. A float over the
// column's last words gives that column the end room, as padding, so the column grows with it,
// the row's content box with the column, and the pane, held in that box, keeps its place at the
// end of the document's scroll; room in `main`, below the row, would carry the pane off it.
// happy-dom lays out no sticky box, so this case reads where the room goes, and the place itself
// is read in a browser (`e2e/foot-sweep.e2e.ts`, the frame's `:stuck` cells).
test("a float over the frame's column gives the end room to the column that states the document scrolls it, beside its stuck Contents pane, and none to the row or main", async () => {
  const height = window.innerHeight;
  boxes = {
    column: { bottom: height, top: 100 },
    words: { bottom: height - 10, top: height - 40 },
  };
  under = "words";
  await afterAPass(
    <main data-testid="main" id="main-content">
      <div data-testid="row" style={{ display: "flex" }}>
        <aside data-testid="contents" style={{ bottom: "auto", position: "sticky", top: 63 }} />
        <div data-testid="column" {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }}>
          <p data-testid="words">The tree's last row.</p>
        </div>
      </div>
    </main>,
    () => {
      expect(screen.getByTestId("column")).toHaveAttribute(MARK_ATTRIBUTE, "padding");
      expect(screen.getByTestId("row")).not.toHaveAttribute(MARK_ATTRIBUTE);
      expect(screen.getByTestId("main")).not.toHaveAttribute(MARK_ATTRIBUTE);
      expect(document.body).not.toHaveAttribute(MARK_ATTRIBUTE);
    }
  );
});

// The end room a float gave a scroller, as the float goes. Room taken away while the reader stands
// in it shortens the scroll under them: `scrollTop` clamps, and what they read jumps by what was
// taken. So room the reader stands in stays until a scroll leaves it, and room they do not stand
// in goes at once. Each case reads the table's box at the foot of the screen, holding 2,000px of
// scroll in a 600px box. The float's band reaches 64px above the box's foot, so its end room is
// 80px, and the reader stands in it with less than 80px of the scroll left: past 1,320px. The
// tall float's band reaches 112px, so with it the room is 128px.

/** The table's box under the float, while `float`, and the tall float, while `tall`. */
function TableUnderFloats({ float, tall = false }: { float: boolean; tall?: boolean }) {
  return (
    <>
      <Table top="auto" />
      <Float active={float} />
      <Float active={tall} testid="tall-float" />
    </>
  );
}

/** Scrolls `box` to `top`, as a reader's scroll does, which the hook hears on `window`. */
function scrollBox(box: HTMLElement, top: number): void {
  box.scrollTop = top;
  window.dispatchEvent(new Event("scroll"));
}

/** Renders the table's box under the floats `floats` names, lets one pass run, and runs `read`
 *  with the box and the render; then scrolls the box to its start, out of any room, so no room
 *  outlives the case, and unmounts. */
async function withTheTable(
  floats: { float: boolean; tall?: boolean },
  read: (box: HTMLElement, view: RenderResult) => Promise<void>
): Promise<void> {
  boxes = { "table-box": { bottom: window.innerHeight, top: 100 } };
  under = "pinned";
  const view = render(<TableUnderFloats {...floats} />);
  const box = screen.getByTestId("table-box");
  try {
    await nextFrame();
    await read(box, view);
  } finally {
    scrollBox(box, 0);
    await nextFrame();
    view.unmount();
  }
}

test("a float that goes while the reader stands in the end room it gave a scroller leaves that room in place", async () => {
  await withTheTable({ float: true }, async (box, view) => {
    expect(box).toHaveAttribute(MARK_ATTRIBUTE);
    const room = box.getAttribute("style");
    expect(room).toContain("80px");
    box.scrollTop = 1400;
    view.rerender(<TableUnderFloats float={false} />);
    await nextFrame();
    expect(box).toHaveAttribute(MARK_ATTRIBUTE);
    expect(box.getAttribute("style")).toBe(room);
  });
});

test("end room kept after its float went stays through a scroll inside it, and goes at the first scroll that leaves it", async () => {
  await withTheTable({ float: true }, async (box, view) => {
    box.scrollTop = 1400;
    view.rerender(<TableUnderFloats float={false} />);
    await nextFrame();
    scrollBox(box, 1350);
    await nextFrame();
    expect(box).toHaveAttribute(MARK_ATTRIBUTE);
    scrollBox(box, 1300);
    await nextFrame();
    expect(box).not.toHaveAttribute(MARK_ATTRIBUTE);
    expect(box.getAttribute("style") ?? "").not.toContain("80px");
  });
});

test("a float that goes while the reader is out of the end room it gave takes that room at once", async () => {
  await withTheTable({ float: true }, async (box, view) => {
    expect(box).toHaveAttribute(MARK_ATTRIBUTE);
    box.scrollTop = 1300;
    view.rerender(<TableUnderFloats float={false} />);
    expect(box).not.toHaveAttribute(MARK_ATTRIBUTE);
  });
});

test("a float that goes while the reader stands in the part of the end room only it needed leaves the room whole until a scroll leaves that part", async () => {
  await withTheTable({ float: true, tall: true }, async (box, view) => {
    expect(box.getAttribute("style")).toContain("128px");
    box.scrollTop = 1400;
    view.rerender(<TableUnderFloats float />);
    await nextFrame();
    expect(box.getAttribute("style")).toContain("128px");
    scrollBox(box, 1340);
    await nextFrame();
    expect(box.getAttribute("style")).toContain("80px");
  });
});

// The document's room is padding, and a page that gives way to it rather than lengthening passes
// the clamp on to the box that grew: the issues table's box is the window less that padding. So
// the document's scroller keeps its room while a box it holds stands within it, however its own
// scroll stands. Here the float lies over a line of `main`, which the document scrolls, and the
// box beside that line holds 2,000px of scroll in a 600px box, as the table's does.

/** `main`, which the document scrolls, holding a line under the float and a box the page gives
 *  way to; the float shows while `float`. */
function PageGivingWay({ float }: { float: boolean }) {
  return (
    <>
      <main data-testid="main" id="main-content" {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }}>
        <p data-testid="line">The last line of the page.</p>
        <div data-testid="given-way" ref={sized(600, 2000)} style={{ overflowY: "auto" }}>
          <p>A row of the table.</p>
        </div>
      </main>
      <Float active={float} />
    </>
  );
}

test("the document's scroller keeps its room while taking it would clamp a box the page gives way to, and loses it at the scroll that leaves that box's end", async () => {
  boxes = { line: { bottom: window.innerHeight, top: window.innerHeight - 20 } };
  under = "line";
  const view = render(<PageGivingWay float />);
  const main = screen.getByTestId("main");
  const box = screen.getByTestId("given-way");
  try {
    await nextFrame();
    expect(main).toHaveAttribute(MARK_ATTRIBUTE, "padding");
    expect(main.style.paddingBottom).toBe("80px");

    // The reader stands at the end of the box, which the page gave way for: the document's own
    // scroll is at its start and would clamp nothing, and the box would be clamped by 80px.
    scrollBox(box, 1400);
    view.rerender(<PageGivingWay float={false} />);
    await nextFrame();
    expect(main).toHaveAttribute(MARK_ATTRIBUTE, "padding");
    expect(main.style.paddingBottom).toBe("80px");

    scrollBox(box, 1300);
    await nextFrame();
    expect(main).not.toHaveAttribute(MARK_ATTRIBUTE);
    expect(main.style.paddingBottom).toBe("");
  } finally {
    scrollBox(box, 0);
    await nextFrame();
    view.unmount();
  }
});
