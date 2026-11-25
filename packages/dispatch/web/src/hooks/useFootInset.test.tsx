import { afterEach, beforeEach, expect, spyOn, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import { type ReactNode, useRef } from "react";

import { nextFrame } from "../__tests__/frames";
import { DOCUMENT_SCROLL_ATTRIBUTE, MARK_ATTRIBUTE, useFootInset } from "./useFootInset";

// happy-dom lays nothing out and has no hit test, so the harness is the geometry. The float, in
// the foot's status layer as Pending changes is, stands 1rem above the foot of the screen; every
// point of it hits the float and then the element a row names (`under`). Each row gives the boxes
// it needs (`boxes`), by `data-testid`, each a top and a bottom edge on the screen, and sizes each
// scroll container's padding box and content (`sized`). Every other element has no box. happy-dom
// computes no initial value for `top` and `bottom`, so each sticky element states its own, as a
// browser's computed style reads them.
const FLOAT_HEIGHT = 48;
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
    if (name === "float") {
      return rect(window.innerHeight - REM - FLOAT_HEIGHT, window.innerHeight - REM, 600, 900);
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

/** The float, a foot element of the status layer. */
function Float() {
  const ref = useRef<HTMLDivElement>(null);
  useFootInset(ref, true, "status");
  return <div data-testid="float" ref={ref} style={{ position: "fixed" }} />;
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
    contents: { bottom: height, top: 63 },
    row: { bottom: height, top: 100 },
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
