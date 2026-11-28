import { expect, spyOn, test } from "bun:test";
import {
  act,
  type RenderHookResult,
  type RenderResult,
  render,
  renderHook,
  screen,
} from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { createRef, type ReactNode, type RefObject, useRef, useState } from "react";

import { disarm } from "../__tests__/armed-press";
import { SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { FootFloat } from "../__tests__/foot-float";
import { nextFrame } from "../__tests__/frames";
import { drawOver, layRowsOut, watchMoves } from "../__tests__/laid-out-rows";
import { DOCUMENT_SCROLL_ATTRIBUTE, footPassNow, MARK_ATTRIBUTE } from "../hooks/useFootInset";
import type { ControlSize } from "./control-names";
import { ARM_ON_PRESS, holdHandOff, useFocusPlace } from "./focus-place";
import { ROW_MARKER } from "./row-marker";
import {
  focusListStop,
  type MarkedList as MarkedListModel,
  type MarkedPlace,
  SELECTION_BAR_TOP_PROPERTY,
  SelectionBar,
  useLastMarkedPlace,
} from "./SelectionBar";

/**
 * Tailwind's own definition of the utility the bar is placed by. happy-dom applies a stylesheet it
 * is given but loads none of the application's generated CSS, so the placement is read from the
 * computed style with this in the document: a bar that stopped drawing over the list - and so
 * started pushing the list's rows down - computes something else here.
 */
const PLACEMENT_CSS = ".fixed{position:fixed}";

test("nothing is drawn while no row is selected", () => {
  const view = render(
    <SelectionBar count={0} label="Selected rows">
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  try {
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(view.container.textContent).toBe("");
  } finally {
    view.unmount();
  }
});

test("the count and the actions are drawn, over the list rather than in it", async () => {
  const user = userEvent.setup();
  const presses: string[] = [];
  const style = document.createElement("style");
  style.textContent = PLACEMENT_CSS;
  document.head.appendChild(style);
  const view = render(
    <SelectionBar count={3} label="Selected rows">
      <button onClick={() => presses.push("clear")} type="button">
        Clear selection
      </button>
    </SelectionBar>
  );
  try {
    const bar = screen.getByRole("toolbar", { name: "Selected rows" });
    expect(bar).toHaveAttribute("data-control", "selection-bar");
    expect(bar.textContent).toContain("3 selected");
    // Over the list: a bar drawn in the page would push every row of the list down as the first
    // row was marked.
    expect(getComputedStyle(bar).position).toBe("fixed");

    await user.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(presses).toEqual(["clear"]);
  } finally {
    view.unmount();
    style.remove();
  }
});

test("a state of the caller's own is said in place of the count, at a count of zero as well", () => {
  const view = render(
    <SelectionBar count={0} label="Selected rows" state="Snoozing 3…">
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  try {
    const bar = screen.getByRole("toolbar", { name: "Selected rows" });
    expect(bar.textContent).toContain("Snoozing 3…");
    expect(bar.textContent).not.toContain("0 selected");
  } finally {
    view.unmount();
  }
});

test("a notice is drawn in the bar, announced, and beside no action", () => {
  const view = render(
    <SelectionBar count={2} label="Selected rows" notice="Two of those are not yours to snooze.">
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  try {
    const bar = screen.getByRole("toolbar", { name: "Selected rows" });
    const notice = screen.getByRole("alert");
    expect(notice.textContent).toBe("Two of those are not yours to snooze.");
    expect(bar.contains(notice)).toBe(true);
    // Its own line: the actions are in a box of their own, which the notice is not in.
    expect(notice.parentElement).toBe(bar);
    expect(screen.getByRole("button", { name: "Clear selection" }).parentElement).not.toBe(bar);
  } finally {
    view.unmount();
  }
});

test("the bar's box takes the page's size: the comfortable padding by default, the compact padding at the compact size", () => {
  const bar = (size?: ControlSize) => (
    <SelectionBar count={2} label="Selected rows" size={size}>
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  const view = render(bar());
  const padding = () =>
    sizeUnder(screen.getByRole("toolbar", { name: "Selected rows" }), SIZE_CSS).padding;
  try {
    // With no size the bar is drawn comfortable, its default.
    expect(padding()).toBe("8px 16px 8px 16px");
    view.rerender(bar("comfortable"));
    expect(padding()).toBe("8px 16px 8px 16px");
    view.rerender(bar("compact"));
    expect(padding()).toBe("4px 8px 4px 8px");
  } finally {
    view.unmount();
  }
});

/** Tailwind's definitions of the utilities the bar's line is drawn by, and of the `sm:` ones the
 *  compact line was once drawn by from 640 px up, read after them as Tailwind orders them: a
 *  compact line that took them again would read differently with them in the document. */
const LINE_CSS =
  ".flex-nowrap{flex-wrap:nowrap}.flex-wrap{flex-wrap:wrap}.overflow-x-auto{overflow-x:auto}";
const SM_LINE_CSS = String.raw`.sm\:flex-wrap{flex-wrap:wrap}.sm\:overflow-x-visible{overflow-x:visible}`;

/** `css`, in the document while `read` runs. */
function withCss<T>(css: string, read: () => T): T {
  const sheet = document.createElement("style");
  sheet.textContent = css;
  document.head.appendChild(sheet);
  try {
    return read();
  } finally {
    sheet.remove();
  }
}

/** Sets happy-dom's window `width` px wide, which its `matchMedia` answers from, and answers the
 *  function that puts the width back. */
function windowWidth(width: number): () => void {
  const window_ = window as unknown as {
    happyDOM: { setViewport(size: { width: number }): void };
    innerWidth: number;
  };
  const was = window_.innerWidth;
  window_.happyDOM.setViewport({ width });
  return () => window_.happyDOM.setViewport({ width: was });
}

// By the architect's ruling of 2026-11-27 18:42Z (INFRA-3135): the compact line, the project
// List's, wraps at every width, in its groups, and nothing in it scrolls sideways, which the
// layout rule reads as a breach; from `sm` up its bar stands at the foot, where the List keeps
// room for its whole height. The comfortable line keeps one line that scrolls sideways.
test("the compact bar's line wraps at every width and scrolls nowhere sideways, and the comfortable line keeps one line that scrolls", () => {
  const lineAt = (size: ControlSize) => {
    const view = render(
      <SelectionBar count={2} label="Selected rows" size={size}>
        <button type="button">Clear selection</button>
      </SelectionBar>
    );
    try {
      const line = screen.getByRole("button", { name: "Clear selection" }).parentElement;
      if (line === null) throw new Error("the bar draws no line around its actions");
      const read = () => {
        const style = getComputedStyle(line);
        return `${style.flexWrap}, ${style.overflowX === "auto" ? "scrolls sideways" : "scrolls nowhere"}`;
      };
      return [withCss(LINE_CSS, read), withCss(LINE_CSS + SM_LINE_CSS, read)];
    } finally {
      view.unmount();
    }
  };
  expect(lineAt("compact")).toEqual(["wrap, scrolls nowhere", "wrap, scrolls nowhere"]);
  expect(lineAt("comfortable")).toEqual(["nowrap, scrolls sideways", "nowrap, scrolls sideways"]);
});

/** Tailwind's definitions of the utilities the bar is placed by below `sm`, and of its `sm:` ones
 *  as they apply from 640 px up, read after them. */
const PLACE_CSS = ".top-14{top:56px}.bottom-4{bottom:16px}";
const SM_PLACE_CSS = String.raw`.sm\:top-auto{top:auto}.sm\:bottom-4{bottom:16px}`;

/** Where `bar` stands with `css` in the document: its computed `top` and `bottom`. */
function placeUnder(bar: Element, css: string): string {
  return withCss(css, () => {
    const style = getComputedStyle(bar);
    return `top ${style.top}, bottom ${style.bottom}`;
  });
}

// Every bar stands under the header below `sm`, over the top of the page, and at the foot of the
// screen from `sm` up: the compact bar, the project List's, as the comfortable bar and the toolbar
// size's do.
test("every bar stands under the header below sm and at the foot from sm up, the compact bar as the comfortable one", () => {
  const placeAt = (size: ControlSize) => {
    const view = render(
      <SelectionBar count={2} label="Selected rows" size={size}>
        <button type="button">Clear selection</button>
      </SelectionBar>
    );
    try {
      const bar = screen.getByRole("toolbar", { name: "Selected rows" });
      return [placeUnder(bar, PLACE_CSS), placeUnder(bar, PLACE_CSS + SM_PLACE_CSS)];
    } finally {
      view.unmount();
    }
  };
  const sizes: readonly ControlSize[] = ["compact", "comfortable", "toolbar"];
  for (const size of sizes) {
    const [phone, wide] = placeAt(size);
    expect(`${size} below sm: ${phone}`).toContain("top 56px");
    expect(`${size} below sm: ${phone}`).not.toContain("bottom 16px");
    expect(`${size} from sm: ${wide}`).toContain("bottom 16px");
    expect(`${size} from sm: ${wide}`).not.toContain("top 56px");
  }
});

// From `sm` up the room the project List keeps at its end is the compact bar's band in the foot
// inset: how far above the screen's bottom edge the bar's top stands, its whole height and the
// 1rem under it, published while the bar is drawn (`SELECTION_BAR_TOP_PROPERTY`), so a bar that
// wraps onto three lines gives the List room of that height. The comfortable bar publishes
// nothing.
test("from sm up the compact bar publishes how far above the screen's foot its top stands while it is drawn, the room of its whole height, and the comfortable bar publishes nothing", () => {
  const restoreWidth = windowWidth(640);
  const property = () =>
    document.documentElement.style.getPropertyValue(SELECTION_BAR_TOP_PROPERTY);
  // Three lines of 36 px controls, two 8 px gaps between them and the compact bar's own 4 px
  // padding above and below.
  let height = 132;
  // The toolbar laid out `height` px tall, 16 px above the screen's bottom edge, as `sm:bottom-4`
  // stands it: happy-dom lays nothing out.
  const own = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.getAttribute("role") !== "toolbar") return own.call(this);
    const bottom = window.innerHeight - 16;
    return new DOMRect(8, bottom - height, 344, height);
  });
  const bar = (size: ControlSize, count: number) => (
    <SelectionBar count={count} label="Selected rows" size={size}>
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  const view = render(bar("compact", 2));
  try {
    expect(property()).toBe("148px");
    // One line: a 36 px control and the padding.
    height = 44;
    act(() => window.dispatchEvent(new Event("resize")));
    expect(property()).toBe("60px");
    view.rerender(bar("compact", 0));
    expect(property()).toBe("");
    view.rerender(bar("comfortable", 2));
    expect(property()).toBe("");
  } finally {
    view.unmount();
    rect.mockRestore();
    restoreWidth();
  }
});

// Below `sm` the compact bar, the project List's, stands under the header, over the top of the
// page, and is no foot element there: it publishes no band, so the List keeps no room at its end
// for it. At the foot below `sm` it stood over the List's table at 390x844, which main's row "a
// refused bulk write retries after reload…" (`e2e/issues-table.e2e.ts`) reads as the phone
// selection toolbar covering the working table.
test("below sm the compact bar stands under the header and publishes no band while it is drawn", () => {
  const restoreWidth = windowWidth(390);
  // happy-dom lays nothing out, so the toolbar is given a box on the screen, three lines tall
  // under the header: a bar that counted as a foot element here would publish its band from it.
  const own = Element.prototype.getBoundingClientRect;
  const rect = spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (
    this: Element
  ) {
    if (this.getAttribute("role") !== "toolbar") return own.call(this);
    return new DOMRect(8, 56, 374, 132);
  });
  const view = render(
    <SelectionBar count={2} label="Selected rows" size="compact">
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  try {
    // Below `sm` only the utilities without `sm:` apply.
    const place = placeUnder(screen.getByRole("toolbar", { name: "Selected rows" }), PLACE_CSS);
    expect(place).toContain("top 56px");
    expect(place).not.toContain("bottom 16px");
    expect(document.documentElement.style.getPropertyValue(SELECTION_BAR_TOP_PROPERTY)).toBe("");
  } finally {
    view.unmount();
    rect.mockRestore();
    restoreWidth();
  }
});

// Firefox makes a box that scrolls and holds controls a Tab stop of its own, which Chromium and
// WebKit do not. The comfortable and toolbar lines scroll sideways where their actions outrun them,
// and every line takes tabindex -1 alike: no stop of its own, while each action in it stays a Tab
// stop.
test("the bar's line takes tabindex -1 at every size, and its actions keep their Tab stops", () => {
  const sizes: readonly ControlSize[] = ["comfortable", "compact", "toolbar"];
  for (const size of sizes) {
    const view = render(
      <SelectionBar count={2} label="Selected rows" size={size}>
        <button type="button">Clear selection</button>
      </SelectionBar>
    );
    try {
      const action = screen.getByRole("button", { name: "Clear selection" });
      expect(action.parentElement?.getAttribute("tabindex")).toBe("-1");
      expect(action.hasAttribute("tabindex")).toBe(false);
    } finally {
      view.unmount();
    }
  }
});

test("the bar hands its own element back and takes focus, for a page whose keymap puts it there", () => {
  const barRef = createRef<HTMLElement>();
  const view = render(
    <SelectionBar barRef={barRef} count={1} label="Selected rows">
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  try {
    expect(barRef.current).toBe(screen.getByRole("toolbar", { name: "Selected rows" }));
    barRef.current?.focus();
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** Three rows, a control of the page's own outside them, and the bar over them with one action:
 *  the page a hand-off is read on. The bar is drawn while `count` is above zero, and offers
 *  **Select all** while `selectAll` is given, as a page's bar does until that press has run. */
function MarkedList({
  count,
  returnFocus,
  selectAll,
}: {
  count: number;
  returnFocus: () => HTMLElement | null;
  selectAll?: () => void;
}): ReactNode {
  return (
    <>
      <button type="button">Filter</button>
      {["CORE-1", "CORE-2", "CORE-3"].map((key) => (
        <input aria-label={`Select ${key}`} key={key} type="checkbox" />
      ))}
      <SelectionBar count={count} label="Selected rows" returnFocus={returnFocus}>
        {selectAll === undefined ? null : (
          <button onClick={selectAll} type="button">
            Select all
          </button>
        )}
        <button type="button">Clear selection</button>
      </SelectionBar>
    </>
  );
}

function rowBox(key: string): HTMLElement {
  return screen.getByRole("checkbox", { name: `Select ${key}` });
}

/** The reader's press on the bar's action named `name`, which leaves the focus on it: the click
 *  a pointer's press makes in Chromium and Firefox. It arms the bar's hand-off (`armOnPress`), so
 *  the commit after it in the same task that takes the bar away, or takes the action out of a bar
 *  that stays, is the reader's doing. */
function pressInBar(name: string): HTMLElement {
  const action = screen.getByRole("button", { name });
  act(() => {
    action.focus();
    action.click();
  });
  return action;
}

test("the bar going while a control inside it holds the focus hands the focus to what returnFocus names", () => {
  // Whether the bar was still in the page when it was asked where the focus goes.
  const asked: string[] = [];
  const returnFocus = () => {
    asked.push(screen.queryByRole("toolbar") === null ? "after the bar went" : "with the bar up");
    return rowBox("CORE-2");
  };
  const view = render(<MarkedList count={2} returnFocus={returnFocus} />);
  try {
    pressInBar("Clear selection");
    view.rerender(<MarkedList count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    // Not the page's body, from which a keyboard reader would start again at its first link.
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toEqual(["after the bar went"]);
  } finally {
    view.unmount();
  }
});

test("the bar going while the focus is outside it moves nothing", () => {
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedList count={2} returnFocus={returnFocus} />);
  try {
    // On a control of the page's own, which stays: the focus stays with it.
    const filter = screen.getByRole("button", { name: "Filter" });
    act(() => filter.focus());
    view.rerender(<MarkedList count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(filter).toHaveFocus();
    // On the body, where the reader had left it before the bar went: nothing armed the bar's
    // hand-off, and the bar never held the focus, so it is not the bar's to hand on.
    view.rerender(<MarkedList count={1} returnFocus={returnFocus} />);
    act(() => filter.blur());
    view.rerender(<MarkedList count={0} returnFocus={returnFocus} />);
    expect(document.activeElement).toBe(document.body);
    expect(asked).toBe(0);
    // The same going after the reader's press on Clear selection, which arms the bar's hand-off
    // and leaves the focus in the bar, does hand it on: what kept it still above is that no press
    // was armed, and the bar did not hold the focus either.
    view.rerender(<MarkedList count={1} returnFocus={returnFocus} />);
    pressInBar("Clear selection");
    view.rerender(<MarkedList count={0} returnFocus={returnFocus} />);
    expect(rowBox("CORE-2")).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a new returnFocus on every render while the bar stays moves nothing, and the one the bar goes under is the one asked", () => {
  const asked: string[] = [];
  const naming = (key: string) => () => {
    asked.push(key);
    return rowBox(key);
  };
  const view = render(<MarkedList count={2} returnFocus={naming("CORE-1")} />);
  try {
    const clear = pressInBar("Clear selection");
    view.rerender(<MarkedList count={2} returnFocus={naming("CORE-3")} />);
    view.rerender(<MarkedList count={1} returnFocus={naming("CORE-1")} />);
    expect(clear).toHaveFocus();
    expect(asked).toEqual([]);
    view.rerender(<MarkedList count={0} returnFocus={naming("CORE-2")} />);
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toEqual(["CORE-2"]);
  } finally {
    view.unmount();
  }
});

test("a control that takes itself out of a bar that stays hands the focus to what returnFocus names", async () => {
  const user = userEvent.setup();
  // Whether the bar was still in the page when it was asked where the focus goes.
  const asked: string[] = [];
  const returnFocus = () => {
    asked.push(screen.queryByRole("toolbar") === null ? "after the bar went" : "with the bar up");
    return rowBox("CORE-2");
  };
  /** One row marked, and **Select all**, whose press marks all three and takes the button out. */
  function SelectingPage(): ReactNode {
    const [all, setAll] = useState(false);
    return (
      <MarkedList
        count={all ? 3 : 1}
        returnFocus={returnFocus}
        selectAll={all ? undefined : () => setAll(true)}
      />
    );
  }
  const view = render(<SelectingPage />);
  try {
    act(() => screen.getByRole("button", { name: "Select all" }).focus());
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Select all" })).not.toBeInTheDocument();
    expect(screen.getByRole("toolbar", { name: "Selected rows" }).textContent).toContain(
      "3 selected"
    );
    // Not the page's body, where taking the pressed button out of the page left it.
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toEqual(["with the bar up"]);
  } finally {
    view.unmount();
  }
});

test("a control the reader moved the focus off moves nothing when it later goes from a bar that stays", () => {
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const selectAll = () => undefined;
  const selectAllButton = () => screen.getByRole("button", { name: "Select all" });
  const view = render(<MarkedList count={1} returnFocus={returnFocus} selectAll={selectAll} />);
  try {
    // On a control of the page's own when Select all goes: the focus stays with it.
    const filter = screen.getByRole("button", { name: "Filter" });
    act(() => selectAllButton().focus());
    act(() => filter.focus());
    view.rerender(<MarkedList count={3} returnFocus={returnFocus} />);
    expect(filter).toHaveFocus();
    // On the body when Select all goes, where the reader left it after moving off Select all: a
    // render of the bar in between saw the button still in the page and no longer focused.
    view.rerender(<MarkedList count={1} returnFocus={returnFocus} selectAll={selectAll} />);
    act(() => selectAllButton().focus());
    act(() => filter.focus());
    view.rerender(<MarkedList count={2} returnFocus={returnFocus} selectAll={selectAll} />);
    act(() => filter.blur());
    view.rerender(<MarkedList count={3} returnFocus={returnFocus} />);
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBeInTheDocument();
    expect(document.activeElement).toBe(document.body);
    expect(asked).toBe(0);
    // The same going after the reader's press on Select all, which arms the bar's hand-off and
    // leaves the focus on it, does hand it on: what kept it still above is that no press was
    // armed, and the reader had moved the focus off besides.
    view.rerender(<MarkedList count={1} returnFocus={returnFocus} selectAll={selectAll} />);
    pressInBar("Select all");
    view.rerender(<MarkedList count={3} returnFocus={returnFocus} />);
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toBe(1);
  } finally {
    view.unmount();
  }
});

// INFRA-3135's review round 1, R10: the bar's hand-off keeps its sight rule. The bar's line of
// actions scrolls sideways, and the reader pressed Clear selection, then, while the press's write
// was out, scrolled the line so far that the focused Clear selection was out of their sight when
// the selection emptied and took the bar away. The bar hands the focus to no row, so the list is
// not scrolled to one the reader did not pick.
test("the bar going with a control the reader scrolled out of sight hands the focus to nothing", async () => {
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedList count={2} returnFocus={returnFocus} />);
  let answered: () => void = () => undefined;
  try {
    const clear = pressInBar("Clear selection");
    // The host holds the press through its write, as the project List's actions hold theirs.
    answered = holdHandOff();
    // The line 300 px wide at the foot of the window, scrolled until Clear selection is 400 px
    // past its left edge.
    const line = clear.parentElement as HTMLElement;
    line.getBoundingClientRect = () => new DOMRect(0, 700, 300, 44);
    clear.getBoundingClientRect = () => new DOMRect(-400, 704, 120, 36);
    act(() => {
      line.dispatchEvent(new Event("scroll"));
    });
    // The armed press reads what the reader can see of the focused control at each frame.
    await act(nextFrame);
    view.rerender(<MarkedList count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
    expect(asked).toBe(0);
  } finally {
    answered();
    disarm();
    view.unmount();
  }
});

/** Three rows, each a `div` carrying the row marker and its mark - where the Inbox draws an
 *  `<li>`, a row by the same rule (`ROW_SELECTOR`) - a control of the page's own outside them,
 *  and the bar over them: the page the hand-off is read on when a refetch, not the reader,
 *  empties the selection. A loss the reader did not cause leaves the focus on the body rather
 *  than pulled into any row. */
function MarkedTable({
  count,
  returnFocus,
}: {
  count: number;
  returnFocus: () => HTMLElement | null;
}): ReactNode {
  return (
    <>
      <button type="button">Filter</button>
      {["CORE-1", "CORE-2", "CORE-3"].map((key) => (
        <div data-inbox-row={key} key={key} {...ROW_MARKER}>
          <input aria-label={`Select ${key}`} type="checkbox" />
        </div>
      ))}
      <SelectionBar count={count} label="Selected rows" returnFocus={returnFocus}>
        <button type="button">Clear selection</button>
      </SelectionBar>
    </>
  );
}

// INFRA-3135's review round 1, follow-up F4: a refetch, not the reader, empties the selection and
// takes the bar away while it holds the focus. With no press of the reader's on the bar or on a
// row's mark before it, the bar hands the focus to no row, so the list is not scrolled to one they
// did not pick: nothing armed the bar's hand-off (`causedByReader`). As the page did before it had
// any hand-off, the focus stays on the body, nothing scrolls, and the page is never asked where it
// goes. One row of the state table of `focus-place.test.ts` (do).
test("the bar emptied by a refetch with no press hands the focus to no row", () => {
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedTable count={2} returnFocus={returnFocus} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
    view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
    expect(moves.scrolled).toEqual([]);
    expect(asked).toBe(0);
  } finally {
    moves.restore();
    view.unmount();
  }
});

// Emptied by the reader's own Clear selection, a click on the bar, which arms the bar's hand-off
// at its root (`armOnPress`): the bar hands the focus on as it always has, to what the page names.
test("the bar emptied by the reader's Clear selection hands the focus to what returnFocus names", () => {
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedTable count={2} returnFocus={returnFocus} />);
  try {
    pressInBar("Clear selection");
    view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toBe(1);
  } finally {
    view.unmount();
  }
});

// INFRA-3135, round 18 (the tester's): emptied by the reader's press on the last marked row's
// mark, the bar hands the focus on too (`pressOnRowMark`). The click arms the bar's hand-off while
// the bar is drawn, though the mark stands outside the bar, and leaves the checkbox unfocused, as
// WebKit does, so the focus is still on Clear selection when the press takes the last row away,
// and the press is on no control of the bar. With `pressOnRowMark` answering false in a scratch
// copy of e9ed2743, this file, `DataTable.test.tsx`, `Inbox.test.tsx` and `IssuesTable.test.tsx`
// passed 254 of 254, and so did the page-own press row of `e2e/table-controls.e2e.ts` in WebKit
// and Firefox (builds Job infra-3135-r18mk): no test read the rule.
test("the bar emptied by the reader's press on the last row's mark hands the focus to what returnFocus names", () => {
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedTable count={1} returnFocus={returnFocus} />);
  try {
    act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
    const mark = rowBox("CORE-1");
    act(() => mark.click());
    view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toBe(1);
  } finally {
    view.unmount();
  }
});

// The Escape that ends an input method's composition in a control inside the bar is the input
// method's (`inputMethodKey`): it commits or cancels what the reader was composing and arms
// nothing, as it arms nothing at every other root (`armOnPress`). Chromium and Firefox send it
// with `isComposing`, and Safari after the composition's end with keyCode 229. So a refetch that
// empties the selection after it takes the bar away as a going nobody caused: the focus stays on
// the body, and the page is never asked where it goes.
test("an Escape that ends an input method's composition in the bar arms nothing, so the bar emptied after it hands the focus to no row", () => {
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  /** Escape pressed on the bar's Clear selection, with what `composing` adds to its keydown. */
  const escapeInBar = (composing: KeyboardEventInit) => {
    const clear = screen.getByRole("button", { name: "Clear selection" });
    act(() => {
      clear.focus();
      clear.dispatchEvent(
        new KeyboardEvent("keydown", { bubbles: true, key: "Escape", ...composing })
      );
    });
  };
  const view = render(<MarkedTable count={2} returnFocus={returnFocus} />);
  try {
    for (const composing of [{ isComposing: true }, { keyCode: 229 }]) {
      escapeInBar(composing);
      view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
      expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
      expect(document.activeElement).toBe(document.body);
      expect(asked).toBe(0);
      view.rerender(<MarkedTable count={2} returnFocus={returnFocus} />);
    }
    // The same going after the reader's own Escape there hands the focus on: what kept it still
    // above is that the key was the input method's.
    escapeInBar({});
    view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toBe(1);
  } finally {
    view.unmount();
  }
});

/** The keys of `EpicList`'s rows, in its order: the reader marks the middle two. */
const EPIC_KEYS = ["CORE-1", "CORE-2", "CORE-3", "CORE-4"];

/** The project List under its Epic filter, as the bar's hand-off reads it once Take out of epic
 *  is pressed (`IssuesTable`): its section, which arms the hand-off at a press inside it
 *  (`ARM_ON_PRESS`), with its toolbar's Filter, then the rows `keys` names, each carrying the row
 *  marker, its mark and its Start next, and each focusable by a script and a pointer but not by
 *  Tab, as the List's rows are (`IssueRow`), then the bar with Take out of epic, drawn while
 *  `count` is above zero. While `held`, Take out of epic's writes are out, and they hold it and
 *  every row's Start next (`aria-disabled`), as the List's run of an action does (`perform`). */
function EpicList({
  count,
  held,
  keys,
  returnFocus,
}: {
  count: number;
  held: boolean;
  keys: readonly string[];
  returnFocus: () => HTMLElement | null;
}): ReactNode {
  const hold = held ? "true" : undefined;
  return (
    <section {...ARM_ON_PRESS}>
      <button type="button">Filter</button>
      {keys.map((key) => (
        <div data-issue-row={key} key={key} tabIndex={-1} {...ROW_MARKER}>
          <input aria-label={`Select ${key}`} type="checkbox" />
          <button aria-disabled={hold} aria-label={`Start next ${key}`} type="button">
            Start next
          </button>
        </div>
      ))}
      <SelectionBar count={count} label="Selected issues" returnFocus={returnFocus}>
        <button aria-disabled={hold} type="button">
          Take out of epic
        </button>
      </SelectionBar>
    </section>
  );
}

/** The Start next of `EpicList`'s row `key`. */
function startNextOf(key: string): HTMLElement {
  return screen.getByRole("button", { name: `Start next ${key}` });
}

/** `EpicList`'s row `key`, the element around its mark and its Start next. */
function epicRow(key: string): HTMLElement {
  const row = startNextOf(key).closest<HTMLElement>("[data-issue-row]");
  if (row === null) throw new Error(`Start next ${key} stands in no row`);
  return row;
}

/** The reader's click on `button` as WebKit makes it, where a pointer's press focuses no button:
 *  its `pointerdown`, then the focus moved onto `around`, the element around the button that
 *  takes it, or with none dropped onto the body, then its `click`. */
function webKitClick(button: HTMLElement, around: HTMLElement | null): void {
  act(() => {
    button.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    if (around !== null) around.focus();
    else if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
    button.click();
  });
}

/** Where the focus rests: the name of what holds it, else the body. */
function focusLanding(): string {
  const focused = document.activeElement;
  if (focused === null || focused === document.body) return "the body";
  return focused.getAttribute("aria-label") ?? focused.textContent ?? "";
}

/** Where the focus rests once the bar has gone (`focusLanding`), and how many times the page was
 *  asked where it goes. */
interface Landing {
  readonly asked: number;
  readonly landed: string;
}

/** A case of `takeOutOfEpic`: what the reader does while Take out of epic's writes are out - with
 *  `redraw`, which draws the bar again where the case asks for a render of it in between - and
 *  where the focus belongs once the bar has gone. */
interface HeldCase {
  readonly whileHeld: (user: UserEvent, redraw: () => void) => Promise<void>;
  readonly belongs: Landing;
}

/**
 * Takes CORE-2 and CORE-3, the two rows marked, out of the epic as the project List's bar does,
 * with the writes held. The reader presses Take out of epic, which arms the bar's hand-off at the
 * List's section and at the bar (`armOnPress`): a click that focuses it first, as Chromium's and
 * Firefox's do, or with `focusing` false one that leaves the focus where it was, as WebKit's does.
 * The run holds Take out of epic, every row's Start next and what answers it (`holdHandOff`), and
 * `whileHeld` runs then, the bar drawn again where `redrawn` says so. Then the writes land: the
 * commit that draws their answer takes the two rows and the bar away together, the rows first, as
 * the List draws its table before its bar, while the rows it keeps still draw their Start next
 * held, which the commit after it lets go. Answers where the focus rests then: the page names
 * CORE-4's mark, the row now at the place of CORE-2, the row marked last.
 */
async function takeOutOfEpic(
  whileHeld: HeldCase["whileHeld"],
  redrawn: boolean,
  focusing: boolean
): Promise<Landing> {
  const user = userEvent.setup();
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-4");
  };
  const list = (count: number, held: boolean, keys: readonly string[] = EPIC_KEYS) => (
    <EpicList count={count} held={held} keys={keys} returnFocus={returnFocus} />
  );
  const view = render(list(2, false));
  try {
    if (focusing) pressInBar("Take out of epic");
    else act(() => screen.getByRole("button", { name: "Take out of epic" }).click());
    view.rerender(list(2, true));
    const answered = holdHandOff();
    await whileHeld(user, () => {
      if (redrawn) view.rerender(list(2, true));
    });
    const kept = without(EPIC_KEYS, "CORE-2", "CORE-3");
    view.rerender(list(0, true, kept));
    answered();
    view.rerender(list(0, false, kept));
    return { asked, landed: focusLanding() };
  } finally {
    view.unmount();
    disarm();
  }
}

/** Each of `cases` taken through `takeOutOfEpic` twice, with no render of the bar between what
 *  the reader does and the bar's going and with one: where the focus rests then (`got`) and where
 *  it belongs (`want`), by the case's name and the render. */
async function eachTakenOut(
  cases: Readonly<Record<string, HeldCase>>,
  focusing = true
): Promise<{ got: Record<string, Landing>; want: Record<string, Landing> }> {
  const got: Record<string, Landing> = {};
  const want: Record<string, Landing> = {};
  for (const [named, { belongs, whileHeld }] of Object.entries(cases)) {
    for (const redrawn of [false, true]) {
      const where = `${named}, ${redrawn ? "the bar drawn again" : "no render of the bar"} before it goes`;
      got[where] = await takeOutOfEpic(whileHeld, redrawn, focusing);
      want[where] = belongs;
    }
  }
  return { got, want };
}

// INFRA-3135, round 27 (the tester's 7d2a23eb): with Take out of epic's writes out, a click or a
// double click on the held Start next of a marked row takes nothing but its hold, so the bar's
// press stays the cause (`armHandOff`), and it moves the focus out of the bar: onto that Start
// next, as Chromium's and Firefox's click does, or, where a pointer's press focuses no button, as
// in WebKit, onto the row around it or onto the body. The writes land, and their answer takes the
// row, and the Start next with it, away in the commit that takes the bar away, the row first. The
// focus goes where Take out of epic alone puts it, to what the page names - the mark of CORE-4,
// the row now at the place of the row marked last - and not to the body, from which a keyboard
// reader would start again at the page's first link. So it does whether or not a render of the
// bar came between the click and the bar's going. In builds Job infra-3135-r27-selection-nh8z7,
// this case's browser row (`e2e/table-controls.e2e.ts`) passed in Chromium and Firefox, and one
// WebKit double click at 1440 left the focus on the body.
test("a click or a double click on a marked row's held Start next while Take out of epic's writes are out hands the focus to what returnFocus names once the bar goes with that row", async () => {
  const handedOn = { asked: 1, landed: "Select CORE-4" };
  const { got, want } = await eachTakenOut({
    "nothing after the press": { belongs: handedOn, whileHeld: async (_user, redraw) => redraw() },
    "a click on the marked row's held Start next": {
      belongs: handedOn,
      whileHeld: async (user, redraw) => {
        await user.click(startNextOf("CORE-2"));
        redraw();
      },
    },
    "a double click on the marked row's held Start next": {
      belongs: handedOn,
      whileHeld: async (user, redraw) => {
        await user.dblClick(startNextOf("CORE-2"));
        redraw();
      },
    },
    "WebKit's double click on the marked row's held Start next, the focus on the body": {
      belongs: handedOn,
      whileHeld: async (_user, redraw) => {
        webKitClick(startNextOf("CORE-2"), null);
        webKitClick(startNextOf("CORE-2"), null);
        redraw();
      },
    },
    "WebKit's double click on the marked row's held Start next, the focus on its row": {
      belongs: handedOn,
      whileHeld: async (_user, redraw) => {
        webKitClick(startNextOf("CORE-2"), epicRow("CORE-2"));
        webKitClick(startNextOf("CORE-2"), epicRow("CORE-2"));
        redraw();
      },
    },
  });
  expect(got).toEqual(want);
});

// The same press and writes, where the bar's going hands on no focus. A click on the held Start
// next of CORE-1, a row the writes keep, leaves the focus on that Start next as the bar goes beside
// it. A click on the toolbar's Filter, which nothing holds, or on a blank spot of the page, after
// the click on the marked row's held Start next, is the reader's next act: it ends Take out of
// epic's press (`armHandOff`), and the focus stays where the reader put it, though the marked row
// goes with the bar. And where the press left the focus off the bar, as WebKit's click does, the
// bar never held the focus the click on the held Start next took, so its going moves nothing
// either. The page is never asked where the focus goes.
test("the bar going after the focus moved onto a held Start next of a row it keeps, after a later click on an unheld control or a blank spot, or after a press that never focused the bar, leaves the focus where the reader put it", async () => {
  const focused = await eachTakenOut({
    "a click on the held Start next of a row the writes keep": {
      belongs: { asked: 0, landed: "Start next CORE-1" },
      whileHeld: async (user, redraw) => {
        await user.click(startNextOf("CORE-1"));
        redraw();
      },
    },
    "a click on the marked row's held Start next, then on the unheld Filter": {
      belongs: { asked: 0, landed: "Filter" },
      whileHeld: async (user, redraw) => {
        await user.click(startNextOf("CORE-2"));
        redraw();
        await user.click(screen.getByRole("button", { name: "Filter" }));
        redraw();
      },
    },
    "a click on the marked row's held Start next, then on a blank spot of the page": {
      belongs: { asked: 0, landed: "the body" },
      whileHeld: async (user, redraw) => {
        await user.click(startNextOf("CORE-2"));
        redraw();
        await user.click(document.body);
        redraw();
      },
    },
  });
  expect(focused.got).toEqual(focused.want);
  const unfocused = await eachTakenOut(
    {
      "a click on the marked row's held Start next after a press that left the focus off the bar": {
        belongs: { asked: 0, landed: "the body" },
        whileHeld: async (user, redraw) => {
          await user.click(startNextOf("CORE-2"));
          redraw();
        },
      },
    },
    false
  );
  expect(unfocused.got).toEqual(unfocused.want);
});

// The same press and writes, with the reader's click on the marked row's held Start next, which
// they then scroll out of their sight before the writes land. The bar going with that row hands
// the focus to nothing, as it does for a control of its own the reader scrolled out of sight
// (`outOfSight`): the list is not scrolled back to a row the reader scrolled away from.
test("a marked row's held Start next the reader clicked and then scrolled out of sight hands the focus to nothing as the bar goes with its row", async () => {
  const { got, want } = await eachTakenOut({
    "a click on the marked row's held Start next, then a scroll past it": {
      belongs: { asked: 0, landed: "the body" },
      whileHeld: async (user, redraw) => {
        const start = startNextOf("CORE-2");
        await user.click(start);
        // The page scrolled down past the row: its Start next stands above the window's top edge.
        start.getBoundingClientRect = () => new DOMRect(0, -400, 120, 32);
        // The armed press reads what the reader can see of the focused control at each frame.
        await act(nextFrame);
        redraw();
      },
    },
  });
  expect(got).toEqual(want);
});

// The same press and writes, with a click on the marked row's held Start next that took no focus
// from the bar, or whose focus the reader moved on before the bar went. Where the focus had left
// the bar for the body before WebKit's click on that Start next, the bar no longer held the focus
// that click took. A key on the Start next that a click focused does not end Take out of epic's
// press, since the press holds that Start next. Where the key then moves the focus to the row
// CORE-3, as a page's keymap can, the focus is where the reader put it, not where the click put
// it. The bar's going hands on nothing in either case, whether or not a render of the bar came
// between: the focus stays on the body, or goes with CORE-3's row, and the page is never asked.
test("a click on a marked row's held Start next that took no focus from the bar, or whose focus a key then moved to another row, hands on nothing as the bar goes", async () => {
  const { got, want } = await eachTakenOut({
    "the focus dropped onto the body, then WebKit's click on the marked row's held Start next": {
      belongs: { asked: 0, landed: "the body" },
      whileHeld: async (_user, redraw) => {
        act(() => screen.getByRole("button", { name: "Take out of epic" }).blur());
        webKitClick(startNextOf("CORE-2"), null);
        redraw();
      },
    },
    "a click on the marked row's held Start next, then a key moving the focus to CORE-3's row": {
      belongs: { asked: 0, landed: "the body" },
      whileHeld: async (user, redraw) => {
        await user.click(startNextOf("CORE-2"));
        await user.keyboard("j");
        act(() => epicRow("CORE-3").focus());
        redraw();
      },
    },
  });
  expect(got).toEqual(want);
});

/** The keys of `PlacedList`'s twenty rows, in its order. */
const PLACED_KEYS = Array.from({ length: 20 }, (_, index) => `CORE-${index + 1}`);

/** Rows, each an `<li>` with its mark, in a list, and the bar over them with one action: the
 *  page the hand-off where the row marked last cannot take the focus is read on
 *  (`useLastMarkedPlace`). `listed` names the rows the list holds, `undrawn` any of them its
 *  window has not drawn, `leaving` the drawn ones a press is taking away, `headings` the rows a
 *  group's heading is drawn right before, and `last` the row marked last, null where no row is
 *  marked. With `cards`, each row draws a card of its own after its mark, an `<article>` and so a
 *  row nested in it, as an Inbox row draws an ask card. With `dismissals`, each row draws a Dismiss
 *  of its own after its mark, as an Inbox row does (`DismissControl`). With `selectAll`, the bar
 *  offers **Select all** after its count, as the project List's offers **Select all N matching**
 *  while fewer rows are marked than match (`countAction`). `head` is the list's sticky head, as
 *  the List names its table's. Its `returnFocus` ends at the list's own stop where the hand-off
 *  moves nothing (`focusListStop`). */
function PlacedList({
  cards = false,
  count,
  dismissals = false,
  drawRow,
  fit,
  head = null,
  headings = [],
  last,
  leaving = [],
  listed,
  selectAll = false,
  undrawn = [],
}: {
  cards?: boolean;
  count: number;
  dismissals?: boolean;
  /** Draws a listed, undrawn row and lands on its mark once drawn (`MarkedList`'s `drawRow`). */
  drawRow?: (id: string, onLand?: (mark: HTMLElement) => void) => () => void;
  fit?: () => void;
  head?: Element | null;
  headings?: readonly string[];
  last: string | null;
  leaving?: readonly string[];
  listed: readonly string[];
  selectAll?: boolean;
  undrawn?: readonly string[];
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  const markOf = (key: string) =>
    list.current?.querySelector<HTMLElement>(`[data-row="${key}"] input`) ?? null;
  const landAtPlace = useLastMarkedPlace({
    control: markOf,
    drawRow,
    fit,
    head: () => head,
    lastMarked: () => last,
    leaving: (key) => leaving.includes(key),
    list: () => list.current,
    order: () => listed,
    shown: count > 0,
  });
  return (
    <>
      <ul aria-label="Rows" ref={list}>
        {listed.flatMap((key) =>
          undrawn.includes(key)
            ? []
            : [
                headings.includes(key) ? (
                  <li data-heading={key} key={`heading-${key}`}>
                    Group
                  </li>
                ) : null,
                <li data-row={key} key={key}>
                  <input aria-label={`Select ${key}`} type="checkbox" />
                  {dismissals ? <button aria-label={`Dismiss ${key}`} type="button" /> : null}
                  {cards ? <article aria-label={`Card of ${key}`} /> : null}
                </li>,
              ]
        )}
      </ul>
      <SelectionBar
        count={count}
        countAction={selectAll ? <button type="button">Select all</button> : undefined}
        label="Selected rows"
        returnFocus={() => (landAtPlace.land() ? null : focusListStop(list.current))}
      >
        <button type="button">Dismiss</button>
      </SelectionBar>
    </>
  );
}

/** `keys` without `taken`. */
function without(keys: readonly string[], ...taken: string[]): string[] {
  return keys.filter((key) => !taken.includes(key));
}

/** The accessible name of what holds the focus, as a string a failed assertion prints whole. */
function focusedName(): string | null {
  return document.activeElement?.getAttribute("aria-label") ?? null;
}

// A press deep in a long list takes away the two rows marked, the row marked last among them.
// The row after them moves up into its place, where the reader can see it, and takes the focus
// with no scroll. The list's first row, nine rows above the window, would scroll the list back
// to its top and throw the reader's place away.
test("a press that takes away the row marked last hands the focus to the row that moves up into its place, with no scroll", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={2} last="CORE-13" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    const landing = rowBox("CORE-14");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// The press took away the row marked last at the window's top, and the row now at its place stands
// under the list's sticky head: its box is in sight, but the page draws the head at its mark's
// centre. No row before that place is in sight either, so the nearest row after it that the reader
// can see takes the focus, with no scroll, rather than a row the head covers.
test("a press that takes away the row marked last, with the row now at its place under the list's sticky head, hands the focus to the nearest row after it in sight", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-10" listed={PLACED_KEYS} />);
  // The head is 60 px tall, over the top of the row at 0 to 100 px.
  const head = document.createElement("div");
  const drawn = drawOver(head, 60, "[data-row]", 9);
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last="CORE-10" listed={without(PLACED_KEYS, "CORE-10")} />);
    // CORE-11 stands at the place under the head; CORE-12, below it, is the nearest row the reader
    // can see.
    const landing = rowBox("CORE-12");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    drawn();
    view.unmount();
    laidOut();
  }
});

for (const edge of ["bottom", "header"] as const) {
  test(`the row at the removed row's place takes focus when 70% of its mark is clipped at the ${edge}`, () => {
    const laidOut = layRowsOut("[data-row]", 9);
    const atBottom = edge === "bottom";
    const height = atBottom ? 730 : 750;
    const head = document.createElement("div");
    head.getBoundingClientRect = () => new DOMRect(0, 0, 200, 70);
    const last = atBottom ? "CORE-17" : "CORE-10";
    const next = atBottom ? "CORE-18" : "CORE-11";
    const view = render(<PlacedList count={1} head={head} last={last} listed={PLACED_KEYS} />);
    const list = screen.getByRole("list", { name: "Rows" });
    list.style.overflowY = "auto";
    list.append(head);
    list.getBoundingClientRect = () => new DOMRect(0, 0, 200, height);
    Object.defineProperties(list, { clientHeight: { value: height }, clientWidth: { value: 200 } });
    const hit = drawOver(head, 70, "[data-row]", 9, { at: height, element: list });
    const scroll = spyOn(list, "scrollBy").mockImplementation(() => {});
    const moves = watchMoves();
    try {
      pressInBar("Dismiss");
      view.rerender(
        <PlacedList count={0} head={head} last={last} listed={without(PLACED_KEYS, last)} />
      );
      const mark = rowBox(next);
      expect(mark).toHaveFocus();
      expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: mark });
      expect<readonly unknown[]>(scroll.mock.calls).toEqual([[{ top: atBottom ? 70 : -70 }]]);
    } finally {
      moves.restore();
      scroll.mockRestore();
      hit();
      view.unmount();
      laidOut();
    }
  });
}

// The row marked last was below the window when the press took it away, so the row that moves
// up into its place is below it too. The focus goes to the nearest row before that place that
// the reader can see, the window's last row, which the window's bottom edge cuts: it is shown
// whole at that edge.
test("a press that takes away the row marked last, with the row now at its place out of sight, hands the focus to the nearest row before it in sight", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-19" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  const scroll = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last="CORE-19" listed={without(PLACED_KEYS, "CORE-19")} />);
    // CORE-20 is drawn 900 px down, under the window's 768 px; CORE-18 at 800 px is under it too.
    const landing = rowBox("CORE-17");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect<readonly unknown[]>(scroll.mock.calls).toEqual([[{ top: 800 - window.innerHeight }]]);
    expect(moves.scrolled).toEqual([]);
  } finally {
    scroll.mockRestore();
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// INFRA-3135, rounds 28 and 29 (the tester's red rows `8b8476d5` and `35bec491`): the rows a press
// is taking away are still listed when the bar goes, so no place is the place yet. The hand-off
// owes the landing while the list names a row as leaving - the list's own stop holds the focus
// meanwhile, so no row of the reader's is landed on and then taken from them - and lands once the
// commit that takes those rows away has run, on the row the list holds at the place then. A focus
// that commit drops to the page's body is nothing the landing has to take back, since the landing
// comes after it.
test("the hand-off owes its landing while the press's rows are still listed, and lands once they have gone", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  const moves = watchMoves();
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    // The host holds the press through its write, as the project List's actions hold theirs.
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    // The row the reader marked last is still listed, so the focus is parked on its own mark with
    // no scroll: their place, for the page and for the list's anchor, until that row goes.
    expect(rowBox("CORE-13")).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({
      options: { preventScroll: true },
      target: rowBox("CORE-13"),
    });
    expect(moves.scrolled).toEqual([]);
    // The commit that takes those rows away is the one that lands: CORE-14 stands at the place.
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(rowBox("CORE-14")).toHaveFocus();
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// INFRA-3135, round 29 (the architect's condition (b) of 2026-11-26 05:5xZ): the park puts the
// focus on the mark of the row marked last, and a mark the reader cannot see would be a checkbox
// their Space toggles out of their sight. The reader marked a row near the list's top and scrolled
// far down it, so its mark stands above the window: the list's own stop holds the focus while the
// landing is owed, with no scroll.
test("a landing owed on a row the reader cannot see parks the focus on the list's own stop", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={1} last="CORE-3" leaving={["CORE-3"]} listed={PLACED_KEYS} />
  );
  const moves = watchMoves();
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(<PlacedList count={0} last="CORE-3" leaving={["CORE-3"]} listed={PLACED_KEYS} />);
    const list = screen.getByRole("list", { name: "Rows" });
    expect(list).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: list });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// A wheel does not move focus off the parked mark of a leaving row. The landing stays owed so
// focus won't fall to the body with that row: the wheel moves no focus and requests no scroll,
// and the commit that removes the departing rows lands on the row now at their place.
test("a wheel of the reader's own leaves the focus on the parked mark of a leaving row, and the landing is made once the rows go", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  const moves = watchMoves();
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    expect(rowBox("CORE-13")).toHaveFocus();
    const from = moves.focused.length;
    document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
    // Read once the act's own handlers have run.
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-13");
    expect(moves.focused.slice(from).map((move) => move.target.getAttribute("aria-label"))).toEqual(
      []
    );
    expect(moves.scrolled).toEqual([]);
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(focusedName()).toBe("Select CORE-14");
    expect(moves.focused.at(-1)?.options).toEqual({ preventScroll: true });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

/** Resolves after a task, once a `setTimeout(…, 0)` queued before it has run. */
function afterTask(): Promise<void> {
  return new Promise((settle) => setTimeout(settle, 0));
}

// INFRA-3135, round 29 (the architect's red team of 2026-11-26 07:3xZ, F2): where the reader's act
// left the focus is read once the act has been answered, and nothing moves the focus before then.
// A page's keymap on the window answers `j` by moving the focus to the row after the one that holds
// it, so a move made before it, on the document's capture, would leave it no row to start from: it
// would go to the list's first row.
test("j from the parked mark of a leaving row moves the focus to the next row, not to the list's first", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  // The page's keymap: `j` moves the focus to the mark of the row after the one in hand, or to
  // the first row's where no row is in hand.
  const keymap = (event: KeyboardEvent) => {
    if (event.key !== "j") return;
    const row = document.activeElement?.closest("[data-row]");
    const next = row?.nextElementSibling ?? document.querySelector("[data-row]");
    next?.querySelector<HTMLElement>("input")?.focus();
  };
  window.addEventListener("keydown", keymap);
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    expect(rowBox("CORE-13")).toHaveFocus();
    act(() => {
      document.activeElement?.dispatchEvent(
        new KeyboardEvent("keydown", { bubbles: true, key: "j" })
      );
    });
    await act(afterTask);
    expect(rowBox("CORE-14")).toHaveFocus();
  } finally {
    window.removeEventListener("keydown", keymap);
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

/** The page's keymap of `PlacedList`: `j` moves the focus to the mark of the row after the one in
 *  hand, and `k` to the mark of the row before it. Answers the function that takes it off. */
function stepKeys(): () => void {
  const keymap = (event: KeyboardEvent) => {
    const row = document.activeElement?.closest("[data-row]");
    const to =
      event.key === "j"
        ? row?.nextElementSibling
        : event.key === "k"
          ? row?.previousElementSibling
          : undefined;
    to?.querySelector<HTMLElement>("input")?.focus();
  };
  window.addEventListener("keydown", keymap);
  return () => window.removeEventListener("keydown", keymap);
}

// The hand-off parks focus on the last marked row's checkbox. Pressing `j` moves focus to another
// leaving row. The landing stays owed so that once both rows are gone, focus moves to the row now
// at their place instead of falling to the body.
test("j from the parked mark into another leaving row leaves the landing owed, and it is made once the rows have gone, never on the body", async () => {
  const user = userEvent.setup();
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-12" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  const keys = stepKeys();
  const moves = watchMoves();
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-12" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    expect(focusedName()).toBe("Select CORE-12");
    await user.keyboard("j");
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-13");
    view.rerender(
      <PlacedList count={0} last="CORE-12" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(focusedName()).toBe("Select CORE-14");
    expect(moves.focused.at(-1)?.options).toEqual({ preventScroll: true });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    keys();
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// The same rule where the rows go before the read: the reader's wheel leaves the focus on the
// parked mark, and the commit that takes the press's rows away comes before that act has been
// answered, so the focus falls to the page's body with the mark. Nothing lands before the read,
// and the read finds the focus on the body because the control that held it went with its row,
// which is none of the reader's doing: the landing is still owed, and is made then, on the row now
// at their place.
test("a wheel whose read comes after the press's rows have gone leaves the landing owed, and it is made at the read", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    expect(focusedName()).toBe("Select CORE-13");
    document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(document.body).toHaveFocus();
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// The same rule's other side: an act of the reader's that leaves the focus on a control that
// stays - the mark of a row the press is not taking away, or a control outside the list - or on
// the page's body by their own doing ends what is owed, and the focus stays where they put it once
// the press's rows have gone. A landing still owed would take it to the row now at their place.
test.each([
  ["k to the row before the parked mark", "Select CORE-12"],
  ["a click on a control outside the list", "Search"],
  ["a click on a blank part of the page", "the page's body"],
] as const)(
  "%s ends what is owed, and the focus stays there once the rows have gone",
  async (what, stays) => {
    const user = userEvent.setup();
    const laidOut = layRowsOut("[data-row]", 9);
    const view = render(
      <PlacedList count={1} last="CORE-13" leaving={["CORE-13"]} listed={PLACED_KEYS} />
    );
    const outside = render(<button aria-label="Search" type="button" />);
    const keys = stepKeys();
    let answered: () => void = () => undefined;
    try {
      pressInBar("Dismiss");
      answered = holdHandOff();
      view.rerender(
        <PlacedList count={0} last="CORE-13" leaving={["CORE-13"]} listed={PLACED_KEYS} />
      );
      expect(focusedName()).toBe("Select CORE-13");
      if (what === "k to the row before the parked mark") await user.keyboard("k");
      else if (what === "a click on a control outside the list") {
        await user.click(screen.getByRole("button", { name: "Search" }));
      } else await user.click(document.body);
      await act(afterTask);
      /** Where the focus stands: the name of what holds it, or the page's body. */
      const standing = () =>
        document.activeElement === document.body ? "the page's body" : focusedName();
      expect(standing()).toBe(stays);
      view.rerender(
        <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-13")} />
      );
      expect(standing()).toBe(stays);
    } finally {
      keys();
      answered();
      disarm();
      outside.unmount();
      view.unmount();
      laidOut();
    }
  }
);

test("a click outside the rows leaves focus on the body when the parked row goes before the read", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const leaving = ["CORE-13"];
  const view = render(
    <PlacedList count={1} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(<PlacedList count={0} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />);
    const parked = rowBox("CORE-13");
    expect(focusedName()).toBe("Select CORE-13");

    act(() => {
      document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      // The browser blurs the mark on a click on blank space. There is no new focusin.
      parked.blur();
      // The click's release: the read comes a task after it.
      document.body.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
    });
    const kept = without(PLACED_KEYS, "CORE-13");
    view.rerender(<PlacedList count={0} last="CORE-13" listed={kept} />);
    expect(parked.isConnected).toBe(false);
    expect(document.activeElement?.tagName).toBe("BODY");
    await act(afterTask);
    expect(focusedName()).toBeNull();
    view.rerender(<PlacedList count={0} last="CORE-13" listed={kept} />);
    expect(document.activeElement?.tagName).toBe("BODY");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

test("window blur ends the key wait so a later keyup does not owe the landing again", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const leaving = ["CORE-13"];
  const view = render(
    <PlacedList count={1} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(<PlacedList count={0} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />);
    const parked = rowBox("CORE-13");
    expect(focusedName()).toBe("Select CORE-13");

    const key = { bubbles: true, code: "KeyJ", key: "j" };
    act(() => {
      parked.dispatchEvent(new KeyboardEvent("keydown", key));
      window.dispatchEvent(new Event("blur"));
      parked.dispatchEvent(new KeyboardEvent("keyup", key));
    });
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-13");
    view.rerender(<PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-13")} />);
    expect(focusedName()).toBeNull();
    expect(document.activeElement?.tagName).toBe("BODY");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

/** What `parkOnLeaving` answers: the view, the mark the bar's hand-off parked the focus on, and
 *  what takes the case down. */
interface Parked {
  /** Ends the writes' hold, disarms the press, and takes the view and its rows' layout down. */
  readonly end: () => void;
  readonly parked: HTMLElement;
  readonly view: RenderResult;
}

/** Draws `PlacedList`'s rows laid out, at `count` marks and with `last` the row marked last,
 *  naming `leaving` as leaving, each row with a Dismiss of its own where `dismissals` says. Then
 *  presses the bar's Dismiss and holds its writes, which name those rows while the bar goes with
 *  its marks: the bar's hand-off parks the focus on the mark of `last` (`parked`). A failure
 *  before the focus is parked takes the case down itself. */
function parkOnLeaving(
  leaving: readonly string[],
  count: number,
  { dismissals = false, last = "CORE-13" }: { dismissals?: boolean; last?: string } = {}
): Parked {
  const laidOut = layRowsOut("[data-row]", 9);
  const page = (marks: number) => (
    <PlacedList
      count={marks}
      dismissals={dismissals}
      last={last}
      leaving={leaving}
      listed={PLACED_KEYS}
    />
  );
  const view = render(page(count));
  let answered: () => void = () => undefined;
  const end = () => {
    answered();
    disarm();
    view.unmount();
    laidOut();
  };
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(page(0));
    const parked = rowBox(last);
    expect(focusedName()).toBe(`Select ${last}`);
    return { end, parked, view };
  } catch (error) {
    end();
    throw error;
  }
}

// A press of the reader's own on another row's own Dismiss while a landing is owed. Chromium and
// Firefox move the focus onto that Dismiss as the button goes down, and the click its release
// makes, about 100 ms later, is what names that row as leaving, as an Inbox row's own Dismiss does
// (`features/inbox/useInboxSelection.ts`). A read a task after the `pointerdown` would find the
// focus on a control of a row no press has named, a control that stays, so nothing would be owed
// again and the row's going would drop the focus onto the body. The read comes a task after the
// `pointerup`, once that click has run: the focus stands in a row the list names as leaving, so
// the landing is owed again, and is made once that row has gone, on the row now at the place of
// the row marked last.
test("a press on another row's own Dismiss while a landing is owed is read after its click, and the landing is made once that row goes", async () => {
  const leaving = ["CORE-12", "CORE-13"];
  /** `PlacedList` with each row's own Dismiss and CORE-13 the row marked last, at `count` marks,
   *  holding `listed` and naming `named` as leaving. */
  const page = (count: number, listed: readonly string[], named: readonly string[]) => (
    <PlacedList count={count} dismissals last="CORE-13" leaving={named} listed={listed} />
  );
  const { end, view } = parkOnLeaving(leaving, 2, { dismissals: true });
  try {
    const own = screen.getByRole("button", { name: "Dismiss CORE-15" });
    act(() => {
      own.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      own.focus();
    });
    // The reader holds the button down as a hand does, well past the task after the press.
    await act(() => Bun.sleep(100));
    act(() => {
      own.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
      own.click();
    });
    // The click names CORE-15 as leaving, in the task of its release.
    view.rerender(page(0, PLACED_KEYS, [...leaving, "CORE-15"]));
    await act(afterTask);
    expect(focusedName()).toBe("Dismiss CORE-15");
    // The bar's rows go first, and CORE-15, still leaving, keeps the focus on its Dismiss.
    view.rerender(page(0, without(PLACED_KEYS, "CORE-12", "CORE-13"), ["CORE-15"]));
    expect(focusedName()).toBe("Dismiss CORE-15");
    // CORE-15 goes, and the focus with its Dismiss: the landing owed again is made.
    view.rerender(page(0, without(PLACED_KEYS, "CORE-12", "CORE-13", "CORE-15"), []));
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    end();
  }
});

// A press's read waits for its own `pointerup`, which the document hears on its capture wherever
// it lands: the reader presses the parked mark, the press's rows go while the button is down and
// take that mark with them, and the button comes up over the row that moved up into their place.
// Nothing lands before the read. The read finds the focus on the body, fallen from a mark that
// went with its row, so the landing is owed again, and is made then.
test("a press whose control leaves the page before its pointerup is read after that pointerup, and the landing is made then", async () => {
  const { end, parked, view } = parkOnLeaving(["CORE-13"], 1);
  try {
    act(() => {
      parked.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    });
    view.rerender(<PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-13")} />);
    expect(parked.isConnected).toBe(false);
    expect(document.activeElement?.tagName).toBe("BODY");
    await act(afterTask);
    expect(document.activeElement?.tagName).toBe("BODY");
    act(() => {
      rowBox("CORE-14").dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
    });
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    end();
  }
});

// A press the browser cancels, as it cancels a touch that turns into a scroll, ends with its
// `pointercancel` and sends no `pointerup`: the read comes a task after the cancel. It finds the
// focus on the parked mark of a leaving row, so the landing is owed again, and is made once the
// rows' going drops the focus onto the body.
test("a press the browser cancels is read after its pointercancel, and the landing is made once the rows go", async () => {
  const { end, parked, view } = parkOnLeaving(["CORE-13"], 1);
  try {
    act(() => {
      parked.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      parked.dispatchEvent(new PointerEvent("pointercancel", { bubbles: true }));
    });
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-13");
    view.rerender(<PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-13")} />);
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    end();
  }
});

// A window `blur` between a press's `pointerdown` and its `pointerup`, as when the reader switches
// windows with the button down, ends the press's wait with no read, as it ends a key's. The press
// ended what was owed, so the rows' going lands nothing. A read at the `pointerup` would find the
// focus on the parked mark of a leaving row and owe the landing again.
test("window blur ends a press's wait so a later pointerup does not owe the landing again", async () => {
  const { end, parked, view } = parkOnLeaving(["CORE-13"], 1);
  try {
    act(() => {
      parked.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      window.dispatchEvent(new Event("blur"));
      parked.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
    });
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-13");
    view.rerender(<PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-13")} />);
    expect(focusedName()).toBeNull();
    expect(document.activeElement?.tagName).toBe("BODY");
  } finally {
    end();
  }
});

// INFRA-3135, round 29 (N2a): a modifier pressed alone acts on nothing - it only changes the key
// pressed with it - so it is none of the reader's acts. Shift on the parked mark of a leaving row
// moves nothing and ends nothing: the landing is still owed, and the commit that takes the press's
// rows away makes it.
test("Shift alone on the parked mark of a leaving row moves nothing, and the landing is still made once the rows go", async () => {
  const user = userEvent.setup();
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  const moves = watchMoves();
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    expect(rowBox("CORE-13")).toHaveFocus();
    const from = moves.focused.length;
    await user.keyboard("{Shift}");
    await act(afterTask);
    expect(moves.focused.slice(from).map((move) => move.target.getAttribute("aria-label"))).toEqual(
      []
    );
    expect(focusedName()).toBe("Select CORE-13");
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    moves.restore();
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// The same rule, for the Tab of Shift+Tab: it is the reader's act, and it starts from the parked
// mark, so the focus goes to the row before it. The reader has moved on, so the rows going then
// land nothing. The reader holds Shift a moment before the Tab, as a hand does.
test("Shift+Tab from the parked mark of a leaving row goes to the row before it, and its Tab is the reader's act", async () => {
  const user = userEvent.setup();
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={1} last="CORE-13" leaving={["CORE-13"]} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-13"]} listed={PLACED_KEYS} />
    );
    expect(rowBox("CORE-13")).toHaveFocus();
    await user.keyboard("{Shift>}");
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-13");
    await user.keyboard("{Tab}{/Shift}");
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-12");
    view.rerender(<PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-13")} />);
    expect(focusedName()).toBe("Select CORE-12");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// On macOS, Chromium and WebKit send no keyup for a key released while Meta is held. So the
// reader's ⌘C on the parked mark - Meta down, C down, Meta up, and no C up - would leave a read
// that waits for C's keyup never run, and the rows' going would drop the focus onto the body. A
// key pressed with Meta is read a task after its keydown instead. The focus stands on the parked
// mark of a leaving row then, so the landing is owed again, and is made once the rows go.
test("⌘C on the parked mark of a leaving row, whose keyup never comes, is read after its keydown, and the landing is made once the rows go", async () => {
  const { end, parked, view } = parkOnLeaving(["CORE-12", "CORE-13"], 2);
  try {
    const meta = { bubbles: true, code: "MetaLeft", key: "Meta" };
    act(() => {
      parked.dispatchEvent(new KeyboardEvent("keydown", { ...meta, metaKey: true }));
      parked.dispatchEvent(
        new KeyboardEvent("keydown", { bubbles: true, code: "KeyC", key: "c", metaKey: true })
      );
      parked.dispatchEvent(new KeyboardEvent("keyup", meta));
    });
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-13");
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    end();
  }
});

// Meta pressed alone is a modifier alone (`LONE_MODIFIERS`), as Shift is: it ends nothing and
// starts no wait for a read. The reader holds ⌘ on the parked mark, and the press's rows go in
// that same task, before a read a task later could run: the landing, still owed, is made by the
// commit that takes them away.
test("Meta alone on the parked mark of a leaving row ends nothing, and the landing is made once the rows go", async () => {
  const { end, parked, view } = parkOnLeaving(["CORE-12", "CORE-13"], 2);
  try {
    act(() => {
      parked.dispatchEvent(
        new KeyboardEvent("keydown", {
          bubbles: true,
          code: "MetaLeft",
          key: "Meta",
          metaKey: true,
        })
      );
    });
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(focusedName()).toBe("Select CORE-14");
    await act(afterTask);
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    end();
  }
});

// Space ticks the checkbox at keyup. The hand-off reads focus after keyup has been answered, so
// Space ticks the parked mark and leaves focus on it without scrolling. The landing stays owed
// until the commit that takes the departing rows away.
test("Space on the parked mark of a leaving row ticks it, and the landing is made once the rows go", async () => {
  const user = userEvent.setup();
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  const moves = watchMoves();
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    const mark = rowBox("CORE-13");
    expect(mark).toHaveFocus();
    // What held the focus as the mark was ticked.
    const tickedOn: (string | null)[] = [];
    mark.addEventListener("change", () => tickedOn.push(focusedName()));
    const from = moves.focused.length;
    await user.keyboard("[Space>]");
    await act(afterTask);
    await user.keyboard("[/Space]");
    await act(afterTask);
    expect(mark).toBeChecked();
    expect(tickedOn).toEqual(["Select CORE-13"]);
    expect(focusedName()).toBe("Select CORE-13");
    expect(moves.focused.slice(from).map((move) => move.target.getAttribute("aria-label"))).toEqual(
      []
    );
    expect(moves.scrolled.length).toBe(0);
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(focusedName()).toBe("Select CORE-14");
    expect(moves.focused.at(-1)?.options).toEqual({ preventScroll: true });
    expect(moves.scrolled.length).toBe(0);
  } finally {
    moves.restore();
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

/** The list's rows `keys`, each an `<li>` with its mark, drawn apart from the hook. */
function rowsApart(keys: readonly string[]): ReactNode {
  return (
    <ul aria-label="Rows">
      {keys.map((key) => (
        <li data-row={key} key={key}>
          <input aria-label={`Select ${key}`} type="checkbox" />
        </li>
      ))}
    </ul>
  );
}

/** The bar's hand-off without its host, over the list `rowsApart` drew, with CORE-13 the row
 *  marked last and named as leaving, and the bar gone. */
function handOffApart(): RenderHookResult<MarkedPlace, unknown> {
  const stop = screen.getByRole("list", { name: "Rows" });
  return renderHook(() =>
    useLastMarkedPlace({
      control: (key) => stop.querySelector<HTMLElement>(`[data-row="${key}"] input`),
      lastMarked: () => "CORE-13",
      leaving: (key) => key === "CORE-13",
      list: () => stop,
      order: () => PLACED_KEYS,
      shown: false,
    })
  );
}

// INFRA-3135, round 29 (N4): the read of where the reader's act left the focus is still to come a
// task after a wheel, and until its keyup after a key, which never comes where the window loses the
// focus first. A settle before it - the hook's unmount, or a new landing owed - ends it unmade, so
// nothing moves the focus after it. The hook runs here without its host and over a list drawn
// apart from it, so the list outlives the hook and a read left behind would move the focus there.
test.each([
  ["a wheel", "the hook's unmount"],
  ["a key", "the hook's unmount"],
  ["a wheel", "a new landing owed"],
  ["a key", "a new landing owed"],
] as const)(
  "%s's read of the focus, ended by %s before it runs, moves nothing",
  async (what, ending) => {
    const laidOut = layRowsOut("[data-row]", 9);
    const rows = render(rowsApart(PLACED_KEYS));
    const hand = handOffApart();
    let mounted = true;
    try {
      // The bar has gone and left the focus on the body: the landing is owed, and parked on the mark.
      act(() => {
        hand.result.current.land();
      });
      const mark = rowBox("CORE-13");
      expect(mark).toHaveFocus();
      if (what === "a wheel") document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
      else
        mark.dispatchEvent(
          new KeyboardEvent("keydown", { bubbles: true, code: "Enter", key: "Enter" })
        );
      if (ending === "the hook's unmount") {
        hand.unmount();
        mounted = false;
      } else {
        act(() => {
          hand.result.current.land();
        });
      }
      if (what === "a key") {
        mark.dispatchEvent(
          new KeyboardEvent("keyup", { bubbles: true, code: "Enter", key: "Enter" })
        );
      }
      await act(afterTask);
      expect(focusedName()).toBe("Select CORE-13");
    } finally {
      if (mounted) hand.unmount();
      rows.unmount();
      laidOut();
    }
  }
);

// The same for a press, whose read waits for its `pointerup`: a settle before it - the hook's
// unmount, or a new landing owed - ends that wait unmade. The pressed mark then leaves the page
// with its row, and the pointerup comes over the body. A read left behind would find the focus on
// the body, fallen from a mark that went with its row, owe the landing again and park the focus on
// the list's own stop. Ended, it moves nothing.
test.each(["the hook's unmount", "a new landing owed"] as const)(
  "a press's read of the focus, ended by %s before its pointerup, moves nothing",
  async (ending) => {
    const laidOut = layRowsOut("[data-row]", 9);
    const rows = render(rowsApart(PLACED_KEYS));
    const hand = handOffApart();
    let mounted = true;
    try {
      // The bar has gone and left the focus on the body: the landing is owed, and parked on the mark.
      act(() => {
        hand.result.current.land();
      });
      const mark = rowBox("CORE-13");
      expect(mark).toHaveFocus();
      act(() => {
        mark.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      });
      if (ending === "the hook's unmount") {
        hand.unmount();
        mounted = false;
      } else {
        act(() => {
          hand.result.current.land();
        });
      }
      rows.rerender(rowsApart(without(PLACED_KEYS, "CORE-13")));
      expect(document.activeElement?.tagName).toBe("BODY");
      act(() => {
        document.body.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
      });
      await act(afterTask);
      expect(document.activeElement?.tagName).toBe("BODY");
    } finally {
      if (mounted) hand.unmount();
      rows.unmount();
      laidOut();
    }
  }
);

// INFRA-3135, round 29 (F8): with nothing leaving, the hand-off lands at once and parks nothing,
// so the focus moves once, onto the mark the landing chose, and never to the list's stop and
// back.
test("a landing with nothing leaving moves the focus once, onto its mark", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={2} last="CORE-13" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    // From the bar's going on: the press's own focus on the bar's action comes before it.
    const from = moves.focused.length;
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(rowBox("CORE-14")).toHaveFocus();
    expect(moves.focused.slice(from).map((move) => move.target.getAttribute("aria-label"))).toEqual(
      ["Select CORE-14"]
    );
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// INFRA-3135, round 29 (F7): the park reads the reader's sight as the landing does, below the
// list's sticky head. The row marked last stands at the window's top, under a 60 px head drawn
// over its mark's centre, so its mark is no place the reader can see, and the list's own stop
// holds the focus while its row is leaving.
test("a landing owed on a row under the list's sticky head parks the focus on the list's own stop", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const head = document.createElement("div");
  const drawn = drawOver(head, 60, "[data-row]", 9);
  const view = render(
    <PlacedList count={1} head={head} last="CORE-10" leaving={["CORE-10"]} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} head={head} last="CORE-10" leaving={["CORE-10"]} listed={PLACED_KEYS} />
    );
    expect(screen.getByRole("list", { name: "Rows" })).toHaveFocus();
  } finally {
    answered();
    disarm();
    drawn();
    view.unmount();
    laidOut();
  }
});

// With no row marked, a press still taking away the first row in sight owes the landing: the
// list's own stop holds the focus meanwhile, and the commit that takes that row away lands on the
// first row in sight then.
test("with no row marked, a press still taking away the first row in sight owes the landing to the first row in sight once it has gone", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={20} last={null} leaving={["CORE-10"]} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(<PlacedList count={0} last={null} leaving={["CORE-10"]} listed={PLACED_KEYS} />);
    expect(screen.getByRole("list", { name: "Rows" })).toHaveFocus();
    view.rerender(<PlacedList count={0} last={null} listed={without(PLACED_KEYS, "CORE-10")} />);
    expect(rowBox("CORE-11")).toHaveFocus();
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// INFRA-3135, round 29 (F3): where a windowed table draws the row marked last itself (`drawRow`),
// the reader's own act takes that landing back, and once the table has landed, an act takes
// nothing back.
test("the reader's own act takes back the landing the page's table owes, and nothing once it has landed", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const asked: string[] = [];
  let abandoned = 0;
  const drawRow = (id: string) => {
    asked.push(id);
    return () => {
      abandoned += 1;
    };
  };
  const view = render(
    <PlacedList
      count={1}
      drawRow={drawRow}
      last="CORE-14"
      listed={PLACED_KEYS}
      undrawn={["CORE-14"]}
    />
  );
  try {
    pressInBar("Dismiss");
    view.rerender(
      <PlacedList
        count={0}
        drawRow={drawRow}
        last="CORE-14"
        listed={PLACED_KEYS}
        undrawn={["CORE-14"]}
      />
    );
    expect(asked).toEqual(["CORE-14"]);
    // The list's own stop holds the focus until the table lands.
    expect(screen.getByRole("list", { name: "Rows" })).toHaveFocus();
    document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
    expect(abandoned).toBe(1);

    // A second press, whose landing the table makes once it draws the row: a later act takes
    // nothing back.
    view.rerender(
      <PlacedList
        count={1}
        drawRow={drawRow}
        last="CORE-14"
        listed={PLACED_KEYS}
        undrawn={["CORE-14"]}
      />
    );
    pressInBar("Dismiss");
    view.rerender(
      <PlacedList
        count={0}
        drawRow={drawRow}
        last="CORE-14"
        listed={PLACED_KEYS}
        undrawn={["CORE-14"]}
      />
    );
    view.rerender(<PlacedList count={0} drawRow={drawRow} last="CORE-14" listed={PLACED_KEYS} />);
    act(() => rowBox("CORE-14").focus());
    view.rerender(<PlacedList count={0} drawRow={drawRow} last="CORE-14" listed={PLACED_KEYS} />);
    document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
    expect(abandoned).toBe(1);
  } finally {
    view.unmount();
    laidOut();
  }
});

// INFRA-3135, round 29 (N1): when a landing is made after a wait (Owed -> Landed) and the row
// marked last is still listed but outside the table's drawn window, the delayed landing asks
// `drawRow` the same way an immediate landing does, rather than falling back to the list's stop.
test("a delayed landing whose row marked last is listed but undrawn asks drawRow and owes its cancellation", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const asked: string[] = [];
  let abandoned = 0;
  const drawRow = (id: string) => {
    asked.push(id);
    return () => {
      abandoned += 1;
    };
  };
  const view = render(
    <PlacedList
      count={1}
      drawRow={drawRow}
      last="CORE-14"
      leaving={["CORE-14"]}
      listed={PLACED_KEYS}
      undrawn={["CORE-14"]}
    />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <PlacedList
        count={0}
        drawRow={drawRow}
        last="CORE-14"
        leaving={["CORE-14"]}
        listed={PLACED_KEYS}
        undrawn={["CORE-14"]}
      />
    );
    expect(asked).toEqual([]);
    expect(screen.getByRole("list", { name: "Rows" })).toHaveFocus();
    // The action settles with CORE-14 still listed and still undrawn: `landNow` calls `drawRow`.
    view.rerender(
      <PlacedList
        count={0}
        drawRow={drawRow}
        last="CORE-14"
        listed={PLACED_KEYS}
        undrawn={["CORE-14"]}
      />
    );
    expect(asked).toEqual(["CORE-14"]);
    expect(screen.getByRole("list", { name: "Rows" })).toHaveFocus();
    document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
    expect(abandoned).toBe(1);
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// A float at the foot keeps the foot pass pending as the bar goes. The last marked row is listed
// but undrawn. Once the pass runs, the list makes its layout current (`fit`), then the table is
// asked to draw and land on the row (`drawRow`). Nothing is asked of the list before the pass.
test("with the foot's pass still to come as the bar goes, the table's own landing waits for it and follows the list's layout made current", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  // The foot's pass reads the screen under the float (`clearScrollers`); happy-dom has no hit
  // test, and no scroller stands there.
  Object.defineProperty(document, "elementsFromPoint", { configurable: true, value: () => [] });
  const asked: string[] = [];
  const drawRow = (id: string) => {
    asked.push(`draw ${id}`);
    return () => undefined;
  };
  const fit = () => {
    asked.push("fit");
  };
  const placed = (count: number) => (
    <PlacedList
      count={count}
      drawRow={drawRow}
      fit={fit}
      last="CORE-14"
      listed={PLACED_KEYS}
      undrawn={["CORE-14"]}
    />
  );
  const view = render(placed(1));
  const float = render(<FootFloat />);
  try {
    await act(nextFrame);
    pressInBar("Dismiss");
    view.rerender(placed(0));
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(asked).toEqual([]);
    await act(nextFrame);
    expect(asked).toEqual(["fit", "draw CORE-14"]);
    // The list's own stop holds the focus until the table lands.
    expect(screen.getByRole("list", { name: "Rows" })).toHaveFocus();
  } finally {
    disarm();
    float.unmount();
    view.unmount();
    Reflect.deleteProperty(document, "elementsFromPoint");
    laidOut();
  }
});

/** Mounts a list at the foot of a document scroller with a float over its last line. */
function mountEndRoomPage({
  id,
  markTop,
  drawRow,
  undrawn,
}: {
  id: string;
  markTop: number;
  drawRow?: MarkedListModel["drawRow"];
  undrawn?: readonly string[];
}) {
  const viewHeight = 900;
  const scrollHeight = 2000;
  let boxHeight = 400;
  const fitHeights: number[] = [];
  const mainId = `${id}-main`;
  const lineId = `${id}-line`;
  const floatId = `${id}-float`;
  const head = document.createElement("div");
  const headBox = spyOn(head, "getBoundingClientRect").mockReturnValue(
    new DOMRect(0, 300, 800, 40)
  );
  const page = (
    count: number,
    float: boolean | "boxless",
    hidden?: readonly string[],
    listed = PLACED_KEYS
  ) => (
    <>
      <main data-testid={mainId} {...{ [DOCUMENT_SCROLL_ATTRIBUTE]: "" }}>
        <PlacedList
          count={count}
          drawRow={drawRow}
          fit={() => {
            const main = document.querySelector<HTMLElement>(`[data-testid="${mainId}"]`);
            const pad = main === null ? 0 : Number.parseFloat(main.style.paddingBottom) || 0;
            boxHeight = 430 - Math.min(30, pad);
            fitHeights.push(boxHeight);
          }}
          head={head}
          last="CORE-10"
          listed={listed}
          undrawn={hidden}
        />
        <p data-testid={lineId}>The last line of the page.</p>
      </main>
      <FootFloat
        active={float !== false}
        boxHeight={float === "boxless" ? 0 : undefined}
        height={viewHeight}
        style={{ position: "fixed" }}
        testId={floatId}
      />
    </>
  );
  const innerHeight = Object.getOwnPropertyDescriptor(window, "innerHeight");
  const docScrollTop = document.documentElement.scrollTop;
  document.documentElement.scrollTop = 0;
  Object.defineProperty(window, "innerHeight", { configurable: true, value: viewHeight });
  const view = render(page(1, true, undrawn));
  const main = screen.getByTestId(mainId);
  const line = screen.getByTestId(lineId);
  const list = screen.getByRole("list", { name: "Rows" });
  const mark = undrawn?.includes("CORE-10") ? null : rowBox("CORE-10");
  list.style.overflowY = "auto";
  list.append(head);
  Object.defineProperties(list, {
    clientHeight: { configurable: true, get: () => boxHeight },
    scrollHeight: { configurable: true, get: () => scrollHeight },
  });
  list.getBoundingClientRect = () => new DOMRect(0, 300, 800, boxHeight);
  line.getBoundingClientRect = () => new DOMRect(0, viewHeight - 20, 800, 20);
  const layOutMark = (node: HTMLElement) => {
    node.getBoundingClientRect = () => new DOMRect(10, markTop, 16, 16);
  };
  if (mark !== null) layOutMark(mark);
  const scrolled = spyOn(list, "scrollBy").mockImplementation(
    (options?: ScrollToOptions | number) => {
      const dy = typeof options === "object" ? (options.top ?? 0) : 0;
      list.scrollTop += dy;
      markTop -= dy;
    }
  );
  Object.defineProperty(document, "elementsFromPoint", {
    configurable: true,
    value: () => {
      const node = screen.queryByTestId(floatId);
      return node === null ? [line] : [node, line];
    },
  });
  return {
    main,
    list,
    scrollHeight,
    get boxHeight() {
      return boxHeight;
    },
    fitHeights,
    layOutMark,
    scrolled,
    rerender: (...props: Parameters<typeof page>) => view.rerender(page(...props)),
    restore: () => {
      list.scrollTop = 0;
      document.documentElement.scrollTop = 0;
      scrolled.mockRestore();
      headBox.mockRestore();
      disarm();
      view.unmount();
      footPassNow();
      if (innerHeight === undefined) Reflect.deleteProperty(window, "innerHeight");
      else Object.defineProperty(window, "innerHeight", innerHeight);
      document.documentElement.scrollTop = docScrollTop;
      Reflect.deleteProperty(document, "elementsFromPoint");
    },
  };
}

// When the bar goes and a scroll box inside main is at its end, removing main's foot room would
// clamp the box. The landing scrolls the box off its end. An early foot pass releases that room,
// and a second fit updates the box before the first painted frame.
test("a landing that scrolls a box off its end releases kept foot room and refits the box in the landing's own task", async () => {
  // CORE-10's checkbox starts 150 px above the sticky head's bottom (340), so the landing
  // scrolls the box up by 150 px, past main's 80 px of kept end room.
  const page = mountEndRoomPage({ id: "end", markTop: 190 });
  const { main, list, scrollHeight, fitHeights, scrolled } = page;
  const mark = rowBox("CORE-10");
  try {
    // Let the foot pass mark main with 80 px of end room while the float is up.
    await act(nextFrame);
    expect(main).toHaveAttribute(MARK_ATTRIBUTE, "padding");
    expect(main.style.paddingBottom).toBe("80px");
    // Put the table box at its scroll's end, then take the float away: main keeps its 80 px room
    // because taking it now would clamp the box.
    list.scrollTop = scrollHeight - page.boxHeight;
    page.rerender(1, false);
    expect(main).toHaveAttribute(MARK_ATTRIBUTE, "padding");
    expect(main.style.paddingBottom).toBe("80px");
    // Press Dismiss and let the bar go: the first fit reads main's kept 80 px room (400 px), the
    // landing scrolls the box 150 px off its end, the early foot pass takes main's room away, and
    // the second fit sets the kept height (430 px) in the landing's own task, before any frame.
    pressInBar("Dismiss");
    page.rerender(0, false);
    expect(mark).toHaveFocus();
    expect(main).not.toHaveAttribute(MARK_ATTRIBUTE);
    expect(main.style.paddingBottom).toBe("");
    expect(page.boxHeight).toBe(430);
    expect(fitHeights).toEqual([400, 430]);
    expect(mark.getBoundingClientRect().top).toBe(340);
    expect<readonly unknown[]>(scrolled.mock.calls).toEqual([[{ top: -150 }]]);
  } finally {
    page.restore();
  }
});

test("the first fit releases stale float room before choosing the row now at the marked place", async () => {
  const page = mountEndRoomPage({ id: "boxless", markTop: 710 });
  const { main, fitHeights } = page;
  try {
    // CORE-11 is below the 400 px box but visible in the 430 px box. CORE-9 is in sight in either.
    page.layOutMark(rowBox("CORE-11"));
    rowBox("CORE-9").getBoundingClientRect = () => new DOMRect(10, 650, 16, 16);
    await act(nextFrame);
    expect(main.style.paddingBottom).toBe("80px");

    pressInBar("Dismiss");
    page.rerender(0, "boxless", undefined, without(PLACED_KEYS, "CORE-10"));
    // The frame's pass still has the old float size. The first fit must use the room without it.
    expect(main.style.paddingBottom).toBe("80px");
    expect(fitHeights).toEqual([]);
    await act(nextFrame);
    expect(focusedName()).toBe("Select CORE-11");
    expect(fitHeights).toEqual([430, 430]);
    expect(main.style.paddingBottom).toBe("");
  } finally {
    page.restore();
  }
});

// When a windowed table draws and focuses its target row in a later render, `onLand` runs the
// early foot pass, refits the box and re-checks the landing in the task that lands it. The kept
// end room is released and the box takes its new height before the next frame.
test("a deferred drawRow landing that scrolls a box off its end releases kept foot room, refits and re-checks in the landing's own task", async () => {
  let pendingLand: ((mark: HTMLElement) => void) | undefined;
  const drawRow = (_id: string, onLand?: (mark: HTMLElement) => void) => {
    pendingLand = onLand;
    return () => {
      pendingLand = undefined;
    };
  };
  // The mark starts at 720–736, 6 px past the bottom of the 430 px box after the refit. The
  // re-check scrolls it by those remaining 6 px.
  const undrawn = ["CORE-10"];
  const page = mountEndRoomPage({ id: "deferred", markTop: 720, drawRow, undrawn });
  const { main, list, scrollHeight, fitHeights, scrolled } = page;
  try {
    await act(nextFrame);
    expect(main).toHaveAttribute(MARK_ATTRIBUTE, "padding");
    list.scrollTop = scrollHeight - page.boxHeight;
    page.rerender(1, false, undrawn);
    expect(main.style.paddingBottom).toBe("80px");
    pressInBar("Dismiss");
    page.rerender(0, false, undrawn);
    expect(pendingLand).toBeDefined();
    expect(fitHeights).toEqual([400]);
    // A later render draws CORE-10 after the table scrolled the box off its end, focuses the mark,
    // and invokes `onLand` in that same task.
    page.rerender(0, false, []);
    const mark = rowBox("CORE-10");
    page.layOutMark(mark);
    act(() => {
      list.scrollTop = 1000;
      mark.focus({ preventScroll: true });
      pendingLand?.(mark);
    });
    expect(main).not.toHaveAttribute(MARK_ATTRIBUTE);
    expect(main.style.paddingBottom).toBe("");
    expect(page.boxHeight).toBe(430);
    expect(fitHeights).toEqual([400, 430]);
    // Re-checked against the 430 px box (300-730): mark at 720-736 scrolled by 6 px to 714-730.
    expect<readonly unknown[]>(scrolled.mock.calls).toEqual([[{ top: 6 }]]);
    expect(mark.getBoundingClientRect().bottom).toBe(730);
  } finally {
    page.restore();
  }
});

// If the bar goes without `returnFocus`, the hook owes a landing only when focus is in a row a
// reader's press is taking away. A live read that clears selection while focus is in an unnamed
// row moves nothing, and a later commit does not land focus elsewhere.
test.each([
  ["an unnamed row that stays", "CORE-11", "Select CORE-11"],
  ["an unnamed row a later live read removes", "CORE-13", null],
] as const)(
  "the bar going for a cause not the reader's with focus in %s owes nothing and a later commit lands nothing",
  (_kind, heldRow, afterRemoval) => {
    const laidOut = layRowsOut("[data-row]", 9);
    const view = render(<PlacedList count={2} last="CORE-13" listed={PLACED_KEYS} />);
    const moves = watchMoves();
    try {
      const mark = rowBox(heldRow);
      act(() => mark.focus());
      const from = moves.focused.length;
      // Another person's update clears the selection and takes the bar away: no press of the
      // reader's is out and no row is named as leaving.
      view.rerender(<PlacedList count={0} last="CORE-13" listed={PLACED_KEYS} />);
      expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
      expect(mark).toHaveFocus();
      expect(moves.focused.slice(from)).toEqual([]);
      expect(moves.scrolled).toEqual([]);
      // A later live read removes CORE-12 and CORE-13: because nothing was owed, nothing lands.
      view.rerender(
        <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
      );
      expect(focusedName()).toBe(afterRemoval);
      expect(moves.focused.slice(from)).toEqual([]);
      expect(moves.scrolled).toEqual([]);
    } finally {
      moves.restore();
      view.unmount();
      laidOut();
    }
  }
);

// INFRA-3135, round 29 (the architect's rulings of 2026-11-26 04:0xZ): a landing is owed only
// until the reader's own next action (`READER_ACTS`), which is the invariant that keeps this
// hand-off from being an app-wide rescue of every focus a page drops. The reader acts while the
// press's rows are still listed, and the act ends what is owed at once, so the commit that takes
// those rows away lands nothing. Where the act left the focus is read once it has been answered:
// a task after its keyup, its pointerup, or the wheel. Here the act leaves the focus on the body,
// and the rows go before that read. A press outside every row is the reader's own choice of the
// body, so its read owes nothing. After a key or a wheel the read finds the focus on the body and
// the parked mark that last held it gone with its row, which is the row's going, not the reader's
// doing: the landing is owed again, and is made at the read, on the row now at their place.
test.each([
  [
    "a key",
    "Select CORE-14",
    () => [
      new KeyboardEvent("keydown", { bubbles: true, code: "KeyJ", key: "j" }),
      new KeyboardEvent("keyup", { bubbles: true, code: "KeyJ", key: "j" }),
    ],
  ],
  [
    "a press",
    "the body",
    () => [
      new PointerEvent("pointerdown", { bubbles: true }),
      new PointerEvent("pointerup", { bubbles: true }),
    ],
  ],
  ["a wheel", "Select CORE-14", () => [new WheelEvent("wheel", { bubbles: true })]],
] as const)(
  "%s of the reader's own ends a landing that is owed, and its read once the rows have gone ends on %s",
  async (_what, ends, acts) => {
    const laidOut = layRowsOut("[data-row]", 9);
    const view = render(
      <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    let answered: () => void = () => undefined;
    try {
      pressInBar("Dismiss");
      answered = holdHandOff();
      view.rerender(
        <PlacedList
          count={0}
          last="CORE-13"
          leaving={["CORE-12", "CORE-13"]}
          listed={PLACED_KEYS}
        />
      );
      expect(rowBox("CORE-13")).toHaveFocus();
      for (const event of acts()) document.dispatchEvent(event);
      // The act leaves the focus on the body. It ended what the going owed, so the rows going now
      // land nothing before the act's read.
      act(() => {
        (document.activeElement as HTMLElement).blur();
      });
      view.rerender(
        <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
      );
      expect(document.body).toHaveFocus();
      await act(afterTask);
      expect(focusLanding()).toBe(ends);
    } finally {
      answered();
      disarm();
      view.unmount();
      laidOut();
    }
  }
);

// INFRA-3135, round 28 (the tester's red row `4d4d86f3`): what the page draws where the bar stood
// takes the room the bar's foot had, which moves the box's own edges in around rows that stayed.
// The landing is read against the box as it stands when it is made, so a mark that room leaves out
// of the reader's sight is never the landing: the nearest row before it that they can see takes
// the focus, shown whole by the least scroll.
test("a landing is read against the box the commit leaves, so a mark the room left out of sight is passed over", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
  );
  const scroll = spyOn(window, "scrollBy").mockImplementation(() => {});
  const height = Object.getOwnPropertyDescriptor(window, "innerHeight");
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    // The host holds the press through its write, as the project List's actions hold theirs.
    answered = holdHandOff();
    view.rerender(
      <PlacedList count={0} last="CORE-13" leaving={["CORE-12", "CORE-13"]} listed={PLACED_KEYS} />
    );
    expect(rowBox("CORE-13")).toHaveFocus();
    Object.defineProperty(window, "innerHeight", { configurable: true, value: 150 });
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    // CORE-14's mark stands at 200 to 300 under a window that ends at 150; CORE-11's, at 100 to
    // 200, is the nearest before it that the reader can see.
    expect(focusedName()).toBe("Select CORE-11");
    expect<readonly unknown[]>(scroll.mock.calls).toEqual([[{ top: 200 - 150 }]]);
  } finally {
    answered();
    disarm();
    if (height === undefined) Reflect.deleteProperty(window, "innerHeight");
    else Object.defineProperty(window, "innerHeight", height);
    scroll.mockRestore();
    view.unmount();
    laidOut();
  }
});

// The reader marked a row near the list's top, then scrolled far down it. The press takes that
// row away, and neither the row now at its place nor any row before it is in sight: each stands
// above the window. The nearest row after the place that the reader can see takes the focus, with
// no scroll, rather than a row above the window or the list itself.
test("a press that takes away the row marked last, with no row in sight at its place or before it, hands the focus to the nearest row after it", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-3" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last="CORE-3" listed={without(PLACED_KEYS, "CORE-3")} />);
    // The window holds CORE-10, cut at its top edge, and CORE-11 whole: CORE-11 is the nearest row
    // after the place the reader can see.
    const landing = rowBox("CORE-11");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// A press that leaves the row marked last in the list, out of sight above the window, is no
// case of the place's: the row's mark takes the focus all the same. The engine's own scroll of a
// focus would centre it; it is shown whole at its nearest edge instead, the least scroll.
test("the row marked last still listed takes the focus out of sight too, shown whole at its nearest edge", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-3" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  const mark = rowBox("CORE-3");
  const scroll = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last="CORE-3" listed={PLACED_KEYS} />);
    expect(mark).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: mark });
    expect<readonly unknown[]>(scroll.mock.calls).toEqual([[{ top: -700 }]]);
    expect(moves.scrolled).toEqual([]);
  } finally {
    scroll.mockRestore();
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// The row marked last, still listed, stands in the window under the list's sticky head, which
// covers the top 60 px of its mark. The engine does not see the head, so its own scroll to the
// nearest edge would move nothing. The mark takes the focus and the page scrolls it out from under
// the head by the least distance, the 60 px.
test("the row marked last still listed under the list's sticky head takes the focus, scrolled out from under the head by the least distance", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  // The head is 60 px tall, over the top of CORE-10, which stands from 0 to 100 px.
  const head = document.createElement("div");
  const headBox = spyOn(head, "getBoundingClientRect").mockReturnValue(new DOMRect(0, 0, 200, 60));
  const view = render(<PlacedList count={1} head={head} last="CORE-10" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  const scrolled = spyOn(window, "scrollBy").mockImplementation(() => {});
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} head={head} last="CORE-10" listed={PLACED_KEYS} />);
    const mark = rowBox("CORE-10");
    expect(mark).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: mark });
    expect<readonly unknown[]>(scrolled.mock.calls).toEqual([[{ top: -60 }]]);
    expect(moves.scrolled).toEqual([]);
  } finally {
    scrolled.mockRestore();
    moves.restore();
    view.unmount();
    headBox.mockRestore();
    laidOut();
  }
});

test("the bar's Dismiss with the row marked last at the window's bottom edge scrolls just the cut, without the root's scroll padding", () => {
  const view = render(<PlacedList count={1} last="CORE-1" listed={["CORE-1"]} />);
  const mark = rowBox("CORE-1");
  let top = window.innerHeight - 6;
  mark.getBoundingClientRect = () => new DOMRect(10, top, 13, 13);
  const list = screen.getByRole("list", { name: "Rows" });
  list.style.overflowY = "auto";
  list.getBoundingClientRect = () => new DOMRect(0, 400, 200, 500);
  Object.defineProperties(list, { clientHeight: { value: 500 }, clientWidth: { value: 200 } });
  const style = document.documentElement.style;
  const padding = style.scrollPaddingBottom;
  style.scrollPaddingBottom = "5rem";
  const scroll = spyOn(window, "scrollBy").mockImplementation((options?: ScrollToOptions) => {
    top -= options?.top ?? 0;
  });
  const scrollBox = spyOn(list, "scrollBy").mockImplementation(() => {});
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last="CORE-1" listed={["CORE-1"]} />);
    expect(mark).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: mark });
    expect(mark.getBoundingClientRect().bottom).toBe(window.innerHeight);
    expect<readonly unknown[]>(scroll.mock.calls).toEqual([[{ top: 7 }]]);
    expect(scrollBox).not.toHaveBeenCalled();
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    scrollBox.mockRestore();
    scroll.mockRestore();
    style.scrollPaddingBottom = padding;
    view.unmount();
  }
});

// Below `sm` the bar stands under the header, fixed over the top of the rows, and the list keeps
// no room for it: at 390x664 the List's wrapped bar stood from 56 to 266 px. A press that takes
// its own control out of a bar that stays, as the List's **Select all N matching** does, hands
// the focus to the checkbox of the row marked last, and for that landing the bar's band covers
// the rows as the list's sticky head does: a checkbox under the bar is shown wholly below its
// bottom edge, by the least scroll of the window. From `sm` up the bar stands at the foot, where
// the list keeps room for it at its end, and the same landing on a checkbox already whole in
// sight scrolls nothing.
test("a landing while the bar stays under the header below sm shows a checkbox under the bar wholly below it by the least scroll, and from sm up, with the bar at the foot, scrolls nothing", async () => {
  /** Where the landing on the checkbox of CORE-11, the row marked last, leaves it on a window
   *  `width` px wide, with the bar `height` px tall where its placement utilities stand it with
   *  `css` in the document, and every scroll of the window. happy-dom applies the utilities and
   *  lays nothing out, so the bar is given that box, and the window's scroll moves the rows,
   *  100 px each, by as much (`layRowsOut`). */
  const landAt = async (width: number, css: string, height: number) => {
    const restoreWidth = windowWidth(width);
    let first = 9;
    let laidOut = layRowsOut("[data-row]", first);
    const scroll = spyOn(window, "scrollBy").mockImplementation((options?: ScrollToOptions) => {
      first += (options?.top ?? 0) / 100;
      laidOut();
      laidOut = layRowsOut("[data-row]", first);
    });
    const view = render(<PlacedList count={1} last="CORE-11" listed={PLACED_KEYS} selectAll />);
    const moves = watchMoves();
    try {
      const bar = screen.getByRole("toolbar", { name: "Selected rows" });
      const [top, bottom] = withCss(css, () => {
        const style = getComputedStyle(bar);
        return [style.top, style.bottom];
      });
      const barTop =
        top === "auto"
          ? window.innerHeight - Number.parseFloat(bottom) - height
          : Number.parseFloat(top);
      bar.getBoundingClientRect = () => new DOMRect(8, barTop, 374, height);
      pressInBar("Select all");
      view.rerender(<PlacedList count={20} last="CORE-11" listed={PLACED_KEYS} />);
      // The landing looks again in a microtask, once the commit it landed in is done.
      await Promise.resolve();
      const mark = rowBox("CORE-11");
      expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBe(bar);
      expect(mark).toHaveFocus();
      expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: mark });
      expect(moves.scrolled).toEqual([]);
      const landed = mark.getBoundingClientRect();
      const stands = bar.getBoundingClientRect();
      return {
        bar: [stands.top, stands.bottom],
        mark: [landed.top, landed.bottom],
        // Each call's arguments as the spy received them: one `ScrollToOptions`.
        scrolls: scroll.mock.calls.map((call): unknown[] => [...call]),
      };
    } finally {
      moves.restore();
      view.unmount();
      scroll.mockRestore();
      laidOut();
      restoreWidth();
    }
  };
  expect({
    "below sm": await landAt(390, PLACE_CSS, 210),
    "from sm up": await landAt(640, PLACE_CSS + SM_PLACE_CSS, 106),
  }).toEqual({
    // Under the header from 56 to 266 px, over the checkbox at 100 to 200 px: the window scrolls
    // up 166 px, which brings the checkbox's top to the bar's bottom edge and no further.
    "below sm": { bar: [56, 266], mark: [266, 366], scrolls: [[{ top: -166 }]] },
    // At the foot, 16 px above the window's 768 px bottom edge, below the checkbox at 100 to
    // 200 px: the checkbox stands whole in sight, and nothing scrolls.
    "from sm up": { bar: [646, 752], mark: [100, 200], scrolls: [] },
  });
});

// A select-all marks no row of the reader's own. Deep in a long list, the reader's press on the
// bar empties the selection while the bar holds the focus. The focus goes to the mark of the
// first row the reader can see, the window's top row, with no scroll. The row above it, whose box
// ends at the window's top edge, shows none of itself. The list's first row, nine rows above the
// window, would scroll the list back to its top and throw the reader's place away.
test("with no row marked, the bar gone with the focus hands it to the first row in sight, with no scroll", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={20} last={null} listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last={null} listed={PLACED_KEYS} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    const landing = rowBox("CORE-10");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// With no row marked, the window's top row is drawn under the list's sticky head: its box is in
// sight, but the page draws the head at its mark's centre. That row is passed over, and the
// focus goes to the row below the head, with no scroll.
test("with no row marked, a row in sight under the list's sticky head is passed over to the next row in sight", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={20} last={null} listed={PLACED_KEYS} />);
  // The head is 60 px tall, over the top of CORE-10, which stands from 0 to 100 px.
  const head = document.createElement("div");
  const drawn = drawOver(head, 60, "[data-row]", 9);
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last={null} listed={PLACED_KEYS} />);
    const landing = rowBox("CORE-11");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    drawn();
    view.unmount();
    laidOut();
  }
});

// With no row marked and no row in sight - the press emptied the list - the list itself takes
// the focus at its own stop, with no scroll, and the focus does not drop onto the page's body.
test("with no row marked and no row left, the bar gone with the focus hands it to the list's own stop", () => {
  const view = render(<PlacedList count={20} last={null} listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    pressInBar("Dismiss");
    view.rerender(<PlacedList count={0} last={null} listed={[]} />);
    const list = screen.getByRole("list", { name: "Rows" });
    expect(list).toHaveFocus();
    expect(list).toHaveAttribute("tabindex", "-1");
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: list });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
  }
});

// Handed on: the page's own hand-off ends a landing that is owed, and the hand-off never takes the
// focus back from a control that stays. While the press's rows are still listed and the focus is
// parked on the mark of the row marked last, the page moves the focus with no act of the reader's,
// as a dialog that opens moves it to its first control, and the commit that takes the rows away
// leaves it there. The page's body and the list's own stop are no control that stays, so a focus
// the page moves to either is still landed, on the row now at the rows' place.
test.each([
  ["a control outside the list", "Confirm"],
  ["the page's body", "Select CORE-14"],
  ["the list's own stop", "Select CORE-14"],
] as const)(
  "a landing owed while the page moves the focus to %s ends on %s once the rows have gone",
  (where, ends) => {
    const leaving = ["CORE-12", "CORE-13"];
    const { end, view } = parkOnLeaving(leaving, 2);
    const dialog = render(<button aria-label="Confirm" type="button" />);
    try {
      act(() => {
        if (where === "a control outside the list") {
          screen.getByRole("button", { name: "Confirm" }).focus();
        } else if (where === "the page's body") {
          rowBox("CORE-13").blur();
        } else {
          focusListStop(screen.getByRole("list", { name: "Rows" }));
        }
      });
      view.rerender(
        <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, ...leaving)} />
      );
      expect(focusedName()).toBe(ends);
    } finally {
      dialog.unmount();
      end();
    }
  }
);

// Cancelled, where the row the reader's act left the focus in stays: `j` from the parked mark moves
// the focus into another row the press names as leaving, so the landing is owed again. A row the
// list names need not go - Take out of epic with no Epic filter keeps its rows listed - and the
// act's end stands only once the hand-off knows that row stays: no row named leaving, and the row
// still listed with the focus in it. The focus then stays where the act left it, and that row's
// later going owes nothing. Where the act's row goes while another row is still named, the focus
// falls with it, and the landing is made once no row is named, on the row now at the place.
test.each([
  ["the names end with the act's row still listed", "Select CORE-13"],
  ["the act's row goes before the names end", "Select CORE-14"],
] as const)(
  "j from the parked mark into another leaving row, where %s, ends on %s",
  async (when, ends) => {
    const user = userEvent.setup();
    const leaving = ["CORE-11", "CORE-12", "CORE-13"];
    const { end, view } = parkOnLeaving(leaving, 3, { last: "CORE-12" });
    const keys = stepKeys();
    try {
      await user.keyboard("j");
      await act(afterTask);
      expect(focusedName()).toBe("Select CORE-13");
      if (when === "the names end with the act's row still listed") {
        view.rerender(<PlacedList count={0} last="CORE-12" listed={PLACED_KEYS} />);
        expect(focusedName()).toBe(ends);
        // What the names' end settled stays settled: the act's row going later lands nothing.
        view.rerender(
          <PlacedList count={0} last="CORE-12" listed={without(PLACED_KEYS, "CORE-13")} />
        );
        expect(focusLanding()).toBe("the body");
      } else {
        // CORE-12 and CORE-13 go while CORE-11 is still named, and the focus falls with CORE-13.
        const kept = without(PLACED_KEYS, "CORE-12", "CORE-13");
        view.rerender(<PlacedList count={0} last="CORE-12" leaving={leaving} listed={kept} />);
        view.rerender(<PlacedList count={0} last="CORE-12" listed={kept} />);
        expect(focusedName()).toBe(ends);
      }
    } finally {
      keys();
      end();
    }
  }
);

/** The rows Dismiss names as leaving in `untickWhileHeld`, CORE-13 the row marked last. */
const UNTICK_PRESS = ["CORE-12", "CORE-13"];

/** `PlacedList` with CORE-13 the row marked last, at `count` marks, holding `listed` and naming
 *  `leaving` as leaving. */
function untickList(
  count: number,
  listed: readonly string[],
  leaving: readonly string[] = []
): ReactNode {
  return <PlacedList count={count} last="CORE-13" leaving={leaving} listed={listed} />;
}

/** Presses Dismiss over `untickList` with CORE-12 and CORE-13 marked, drawn in `view`, and holds
 *  its writes, which name both rows as leaving while the bar stays up. Then the reader unticks
 *  CORE-13 with a click, which leaves the focus on its checkbox, in a row the press names as
 *  leaving, while CORE-12's mark keeps the bar up. Answers what ends the writes' hold. */
async function untickWhileHeld(view: RenderResult): Promise<() => void> {
  const user = userEvent.setup();
  pressInBar("Dismiss");
  const answered = holdHandOff();
  view.rerender(untickList(2, PLACED_KEYS, UNTICK_PRESS));
  await user.click(rowBox("CORE-13"));
  view.rerender(untickList(1, PLACED_KEYS, UNTICK_PRESS));
  expect(focusedName()).toBe("Select CORE-13");
  expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBeInTheDocument();
  return answered;
}

// The bar's going owes the landing where the focus stands in a row the list names as leaving, and
// that row can go in the very commit that takes the bar away: the reader unticked the row marked
// last while another mark kept the bar up, and the press's writes then land and take both rows and
// the bar away together, which drops the focus onto the body before the hand-off reads it. The
// hand-off remembered the checkbox in its leaving row while the bar was drawn, so the bar's going
// finds it gone from the page with its row, owes the landing, and lands on the row now at the
// rows' place rather than leave the reader on the body.
test("a click that unticks the row marked last while the press's writes are out lands on the row now at its place when the writes take its row and the bar away together", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(untickList(2, PLACED_KEYS));
  let answered: () => void = () => undefined;
  try {
    answered = await untickWhileHeld(view);
    view.rerender(untickList(0, without(PLACED_KEYS, ...UNTICK_PRESS)));
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(focusLanding()).toBe("Select CORE-14");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// The remembered control is the reader's place only while the focus stays with it, so a stale
// memory owes nothing. As above, the reader unticked the row marked last with a click and the
// focus stands on its checkbox in a leaving row; then the focus leaves it before the bar goes. The
// page drops it onto the body with the checkbox still in the page, and the bar goes before the
// rows; or the page moves it to a control outside the rows, as a dialog's first control takes it,
// and then onto the body; or the reader's own click on a blank part of the page puts it there. The
// bar's going, and the rows' going, move nothing.
test.each([
  "the page drops the focus onto the body and the bar goes before the rows",
  "the page moves the focus to a control outside the rows and then onto the body",
  "the reader clicks a blank part of the page",
] as const)(
  "after a click that unticks the row marked last while the press's writes are out, where %s, the bar's going moves nothing",
  async (then) => {
    const user = userEvent.setup();
    const laidOut = layRowsOut("[data-row]", 9);
    const view = render(untickList(2, PLACED_KEYS));
    const outside = render(<button aria-label="Confirm" type="button" />);
    const barFirst =
      then === "the page drops the focus onto the body and the bar goes before the rows";
    let answered: () => void = () => undefined;
    try {
      answered = await untickWhileHeld(view);
      if (barFirst) {
        act(() => rowBox("CORE-13").blur());
      } else if (
        then === "the page moves the focus to a control outside the rows and then onto the body"
      ) {
        act(() => {
          const control = screen.getByRole("button", { name: "Confirm" });
          control.focus();
          control.blur();
        });
      } else {
        await user.click(document.body);
      }
      expect(focusLanding()).toBe("the body");
      const moves = watchMoves();
      try {
        if (barFirst) {
          view.rerender(untickList(0, PLACED_KEYS, UNTICK_PRESS));
          expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
          expect(focusLanding()).toBe("the body");
        }
        view.rerender(untickList(0, without(PLACED_KEYS, ...UNTICK_PRESS)));
        expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
        expect(focusLanding()).toBe("the body");
        expect(moves.focused.map((move) => move.target.getAttribute("aria-label"))).toEqual([]);
      } finally {
        moves.restore();
      }
    } finally {
      answered();
      disarm();
      outside.unmount();
      view.unmount();
      laidOut();
    }
  }
);

// An act of the reader's that leaves the focus where it was keeps the remembered control: the act
// forgets it only where, once the act is read, the focus no longer stands on it. ⌘C, whose keyup
// macOS never sends while Meta is held, and a wheel move no focus and make no commit, so once each
// is read the focus still stands on the checkbox of the row marked last, in a row the press names
// as leaving. The writes then take its row and the bar away together, which drops the focus onto
// the body before the hand-off reads it: the going owes the landing, and it is made on the row now
// at the rows' place rather than left on the body.
test.each(["⌘C, whose keyup never comes,", "a wheel"] as const)(
  "%s on the checkbox of the row marked last while the press's writes are out keeps it remembered, and the landing is made on the row now at its place when the writes take its row and the bar away together",
  async (how) => {
    const laidOut = layRowsOut("[data-row]", 9);
    const view = render(untickList(2, PLACED_KEYS));
    let answered: () => void = () => undefined;
    try {
      pressInBar("Dismiss");
      answered = holdHandOff();
      view.rerender(untickList(2, PLACED_KEYS, UNTICK_PRESS));
      const mark = rowBox("CORE-13");
      act(() => mark.focus());
      act(() => {
        if (how === "a wheel") {
          mark.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
          return;
        }
        const meta = { bubbles: true, code: "MetaLeft", key: "Meta" };
        mark.dispatchEvent(new KeyboardEvent("keydown", { ...meta, metaKey: true }));
        mark.dispatchEvent(
          new KeyboardEvent("keydown", { bubbles: true, code: "KeyC", key: "c", metaKey: true })
        );
        mark.dispatchEvent(new KeyboardEvent("keyup", meta));
      });
      // Read once the act has been answered.
      await act(afterTask);
      expect(focusedName()).toBe("Select CORE-13");
      view.rerender(untickList(0, without(PLACED_KEYS, ...UNTICK_PRESS)));
      expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
      expect(focusLanding()).toBe("Select CORE-14");
    } finally {
      answered();
      disarm();
      view.unmount();
      laidOut();
    }
  }
);

// The landing a going owes after a wheel is chosen from what the reader sees after that wheel, so
// it never scrolls back to where they scrolled away from. The wheel over the remembered checkbox of
// the row marked last scrolls the list on, past that row and the row after the rows the press is
// taking away: the row at the window's top is the sixteenth, or, scrolled further still, no row is
// in sight. The writes then take the rows and the bar away together, and the landing is made on the
// nearest row after the place that the reader can see, else on the list's own stop, with no scroll.
test.each([
  ["rows after the place in sight", 15, "Select CORE-18"],
  ["no row in sight", 40, "Rows"],
] as const)(
  "a wheel that scrolls the remembered checkbox of the row marked last out of sight, with %s, lands from the sight after it once the writes take the rows and the bar away, and scrolls nothing back",
  async (_sight, first, landed) => {
    let laidOut = layRowsOut("[data-row]", 9);
    const view = render(untickList(2, PLACED_KEYS));
    const scroll = spyOn(window, "scrollBy").mockImplementation(() => {});
    let answered: () => void = () => undefined;
    try {
      pressInBar("Dismiss");
      answered = holdHandOff();
      view.rerender(untickList(2, PLACED_KEYS, UNTICK_PRESS));
      const mark = rowBox("CORE-13");
      act(() => mark.focus());
      act(() => {
        mark.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
      });
      // The wheel's scroll: the row at `first` in the list's order stands at the window's top now.
      laidOut();
      laidOut = layRowsOut("[data-row]", first);
      // Read once the act has been answered.
      await act(afterTask);
      expect(focusedName()).toBe("Select CORE-13");
      const moves = watchMoves();
      try {
        view.rerender(untickList(0, without(PLACED_KEYS, ...UNTICK_PRESS)));
        expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
        expect(focusLanding()).toBe(landed);
        expect(moves.focused.at(-1)?.options).toEqual({ preventScroll: true });
        expect(moves.scrolled.length).toBe(0);
        expect(scroll.mock.calls).toEqual([]);
      } finally {
        moves.restore();
      }
    } finally {
      scroll.mockRestore();
      answered();
      disarm();
      view.unmount();
      laidOut();
    }
  }
);

/** One row drawn in a list's place as a view of its own, as the Inbox draws the row it opens below
 *  1280 px (`features/inbox/ReadingView.tsx`): a `<div>` around the row's mark, which carries the
 *  row marker only where `marker` says so, and the bar over it with one action. The view is drawn
 *  for `open`, the one row the list holds, and is given to the hand-off as that row (`MarkedList`'s
 *  `row`) only where `gives` says so; the bar is handed what the hand-off answers of its rows
 *  (`MarkedPlace`'s `rows`). CORE-1 is the row marked last. Where `held` is given, the view's mark
 *  is ticked and the view draws a Dismiss of its own, which the bar's Dismiss holds while
 *  `held`. */
function OpenRowView({
  count,
  gives,
  held,
  leaving = [],
  marker,
  open,
}: {
  count: number;
  gives: boolean;
  held?: boolean;
  leaving?: readonly string[];
  marker: boolean;
  open: string;
}): ReactNode {
  const view = useRef<HTMLDivElement>(null);
  const viewOf = (key: string) => (key === open ? view.current : null);
  const landAtPlace = useLastMarkedPlace({
    control: (key) => viewOf(key)?.querySelector<HTMLElement>("input") ?? null,
    lastMarked: () => "CORE-1",
    leaving: (key) => leaving.includes(key),
    list: () => view.current,
    order: () => [open],
    row: gives ? viewOf : undefined,
    shown: count > 0,
  });
  return (
    <>
      <div key={open} ref={view} {...(marker ? ROW_MARKER : {})}>
        <input aria-label={`Select ${open}`} defaultChecked={held !== undefined} type="checkbox" />
        {held === undefined ? null : (
          <button aria-disabled={held || undefined} aria-label={`Dismiss ${open}`} type="button" />
        )}
      </div>
      <SelectionBar
        count={count}
        label="Selected rows"
        returnFocus={() => (landAtPlace.land() ? null : focusListStop(view.current))}
        rows={landAtPlace.rows}
      >
        <button type="button">Dismiss</button>
      </SelectionBar>
    </>
  );
}

// A row a list draws as an element no row marker names counts as a row only where the list gives
// that element (`MarkedList`'s `row`). The press parks the focus on the mark of the row it is
// taking away, and a wheel of the reader's there is read once answered: in a row the press names
// as leaving, the landing stays owed and is made on the next row's mark once the row has gone. A
// list that gives no `row` reads the row around the mark (`ROW_SELECTOR`): the same element with
// no marker is in no row, so the wheel's end stands and the focus falls to the body with the view;
// with the marker on it, it is the row, and the landing is made.
test.each([
  ["an element the list gives as the row", "Select CORE-2", true, false],
  ["an element the list does not give, with no row marker", "the body", false, false],
  ["an element the list does not give, with the row marker", "Select CORE-2", false, true],
] as const)(
  "a wheel on the parked mark of a leaving row drawn as %s ends on %s once the row goes",
  async (_drawn, ends, gives, marker) => {
    const view = render(<OpenRowView count={1} gives={gives} marker={marker} open="CORE-1" />);
    let answered: () => void = () => undefined;
    try {
      pressInBar("Dismiss");
      answered = holdHandOff();
      view.rerender(
        <OpenRowView count={0} gives={gives} leaving={["CORE-1"]} marker={marker} open="CORE-1" />
      );
      expect(focusLanding()).toBe("Select CORE-1");
      document.dispatchEvent(new WheelEvent("wheel", { bubbles: true }));
      // Read once the act's own handlers have run.
      await act(afterTask);
      expect(focusLanding()).toBe("Select CORE-1");
      // CORE-1 has gone, and the view is drawn for CORE-2, the next row.
      view.rerender(<OpenRowView count={0} gives={gives} marker={marker} open="CORE-2" />);
      expect(focusLanding()).toBe(ends);
    } finally {
      answered();
      disarm();
      view.unmount();
    }
  }
);

// A press on a control the bar's running action holds in a marked row counts as the bar's own
// press, and the row is the element the list gives (`MarkedList`'s `row`) where no row marker
// makes it one: the bar hands the press it arms at its root the rows the hand-off answers
// (`MarkedPlace`'s `rows`), such as a reading view a page draws for the row it shows. The view's
// own mark, its first checkbox, is ticked. One press on the held control, after which the engine
// leaves the focus on the body, as WebKit does after a click on a button, takes nothing but the
// hold, and the bar's press stays the cause: once the write lands and the bar goes with the row,
// the focus lands on the next row's mark in the view.
test("a press on a control the bar's action holds in a marked row the list gives, with no row marker, counts as the bar's press, and the landing is made on the next row's mark once the row goes", async () => {
  const view = render(<OpenRowView count={1} gives held={false} marker={false} open="CORE-1" />);
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(
      <OpenRowView count={1} gives held leaving={["CORE-1"]} marker={false} open="CORE-1" />
    );
    const own = screen.getByRole("button", { name: "Dismiss CORE-1" });
    expect(own.getAttribute("aria-disabled")).toBe("true");
    act(() => {
      own.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
      own.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
      own.click();
    });
    await act(afterTask);
    expect(focusLanding()).toBe("the body");
    // The write lands: CORE-1 goes with the bar, and the view is drawn for CORE-2, the next row.
    view.rerender(<OpenRowView count={0} gives held={false} marker={false} open="CORE-2" />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(focusLanding()).toBe("Select CORE-2");
  } finally {
    answered();
    disarm();
    view.unmount();
  }
});

// With a landing owed (a bar press whose writes are held), a pointer press on empty space inside
// the bar's `tabindex="-1"` line counts as the reader's own act, never as the body and never as a
// leaving row: the owed landing is not made over that focus while the bar stays. A focus moved
// onto that box without an act likewise ends the owed landing under the Handed on rule.
test.each([
  "a pointer press on empty space in the bar's line",
  "focus moved onto the bar's line",
] as const)("with a landing owed, %s ends the landing unmade while the bar stays", async (how) => {
  const laidOut = layRowsOut("[data-row]", 9);
  const leaving = ["CORE-12", "CORE-13"];
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    // The bar goes while CORE-12 and CORE-13 are still leaving: focus is parked and landing is owed.
    view.rerender(<PlacedList count={0} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />);
    expect(focusedName()).toBe("Select CORE-13");
    // The bar is drawn again (e.g. another mark stays) while the leaving rows have not gone yet.
    view.rerender(<PlacedList count={1} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />);
    const line = screen.getByRole("button", { name: "Dismiss" }).parentElement;
    if (line === null) throw new Error("the bar draws no line around its actions");
    expect(line.getAttribute("tabindex")).toBe("-1");
    if (how === "a pointer press on empty space in the bar's line") {
      act(() => {
        line.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
        line.focus();
        line.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
        line.click();
      });
      await act(afterTask);
    } else {
      act(() => line.focus());
    }
    expect(document.activeElement === line).toBe(true);
    // The leaving rows leave while the bar stays: no landing is made over the bar's line.
    view.rerender(
      <PlacedList count={1} last="CORE-13" listed={without(PLACED_KEYS, ...leaving)} />
    );
    expect(document.activeElement === line).toBe(true);
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// With a bar press whose writes are held while the bar stays drawn, a pointer press on empty space
// inside the bar's `tabindex="-1"` line focuses that line without landing over it while the bar
// stays, and once the writes finish and the bar goes with the rows, the focus is handed on as from
// the bar (to the row now at the leaving rows' place), never left on the body.
test("a pointer press on empty space in the bar's line while writes are held hands focus on from the bar when the bar goes with the rows", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const leaving = ["CORE-12", "CORE-13"];
  const view = render(
    <PlacedList count={2} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />
  );
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    // The bar stays drawn while the writes are out, with CORE-12 and CORE-13 named as leaving.
    view.rerender(<PlacedList count={2} last="CORE-13" leaving={leaving} listed={PLACED_KEYS} />);
    const line = screen.getByRole("button", { name: "Dismiss" }).parentElement;
    if (line === null) throw new Error("the bar draws no line around its actions");
    expect(line.getAttribute("tabindex")).toBe("-1");
    act(() => {
      line.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
      line.focus();
      line.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
      line.click();
    });
    expect(document.activeElement === line).toBe(true);
    // Wait past the transient frame and task an unheld press would expire in: while the bar stays,
    // the focus remains on the bar's line and the held press stays live.
    for (const _ of [0, 1]) {
      await act(nextFrame);
      await act(afterTask);
    }
    expect(document.activeElement === line).toBe(true);
    // The writes settle and the bar goes with the rows: focus is handed on as from the bar.
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, ...leaving)} />
    );
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(focusLanding()).toBe("Select CORE-14");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

/** The rows `listed` names, each an `<li>` with its mark, ticked where `marked` names it, and a
 *  Priority of its own, as the project List's table draws its rows (`DataTable`): the list arms the
 *  hand-off at a press inside it (`ARM_ON_PRESS`), and its own hand-off counts each row's controls
 *  in that row and walks back over the page's main content from the place a row that went left
 *  (`useFocusPlace`). A child of `HandOffRows`, as the table is a child of the List that holds the
 *  bar's hand-off, so its hand-off reads each commit before the bar's does. */
function RowsWithHandOff({
  listRef,
  listed,
  marked,
}: {
  listRef: RefObject<HTMLUListElement | null>;
  listed: readonly string[];
  marked: readonly string[];
}): ReactNode {
  const noteFocus = useFocusPlace(
    (element) => element.closest("[data-row]"),
    (row) => (row.isConnected && row instanceof HTMLElement ? row : null)
  );
  return (
    <ul aria-label="Rows" {...ARM_ON_PRESS} onFocus={noteFocus} ref={listRef}>
      {listed.map((key) => (
        <li data-row={key} key={key}>
          <input
            aria-label={`Select ${key}`}
            defaultChecked={marked.includes(key)}
            type="checkbox"
          />
          <button aria-label={`Priority ${key}`} type="button" />
        </li>
      ))}
    </ul>
  );
}

/** `RowsWithHandOff` in the page's main content with the bar over them and its hand-off, at
 *  `count` marks, with CORE-13 the row marked last and `leaving` named as leaving. */
function HandOffRows({
  count,
  leaving = [],
  listed,
  marked,
}: {
  count: number;
  leaving?: readonly string[];
  listed: readonly string[];
  marked: readonly string[];
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  const landAtPlace = useLastMarkedPlace({
    control: (key) => list.current?.querySelector<HTMLElement>(`[data-row="${key}"] input`) ?? null,
    lastMarked: () => "CORE-13",
    leaving: (key) => leaving.includes(key),
    list: () => list.current,
    order: () => listed,
    shown: count > 0,
  });
  return (
    <main>
      <RowsWithHandOff listRef={list} listed={listed} marked={marked} />
      <SelectionBar
        count={count}
        label="Selected rows"
        returnFocus={() => (landAtPlace.land() ? null : focusListStop(list.current))}
      >
        <button type="button">Dismiss</button>
      </SelectionBar>
    </main>
  );
}

/** The reader's click on a row's `mark` as Chromium and Firefox make it, all in one task: its
 *  `pointerdown`, the focus it puts on the mark, its `pointerup`, and its `click`, which unticks a
 *  ticked mark. The click arms the press on the mark (`SelectionBar`, `ARM_ON_PRESS`), live a
 *  frame and a task on. */
function clickMark(mark: HTMLElement): void {
  act(() => {
    mark.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
    mark.focus();
    mark.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
    mark.click();
  });
}

// While the bar's landing is owed, it wins over the row hand-off that a press on a marked row's
// mark arms. The reader unticks the row marked last with a click while the press's writes are out
// and another mark keeps the bar up, and the writes land within the frame and the task the click
// keeps its press live for, taking both rows and the bar away together. The list's own hand-off
// reads that commit first, as the project List's table does, and would hand the focus the row took
// with it to the Priority before the rows' place; the bar's going owes its landing, so the row
// hand-off moves nothing, and the landing puts the focus on the mark of the row now at the place.
test("a click that unticks the row marked last while the press's writes are out, with its row and the bar then taken away together within the click's frame, lands on the mark of the row now at its place, not where the list's own hand-off would", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const page = (count: number, listed: readonly string[], leaving: readonly string[] = []) => (
    <HandOffRows count={count} leaving={leaving} listed={listed} marked={UNTICK_PRESS} />
  );
  const view = render(page(2, PLACED_KEYS));
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(page(2, PLACED_KEYS, UNTICK_PRESS));
    clickMark(rowBox("CORE-13"));
    expect(rowBox("CORE-13")).not.toBeChecked();
    view.rerender(page(1, PLACED_KEYS, UNTICK_PRESS));
    expect(focusedName()).toBe("Select CORE-13");
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBeInTheDocument();
    view.rerender(page(0, without(PLACED_KEYS, ...UNTICK_PRESS)));
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(focusedName()).toBe("Select CORE-14");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// With no landing of the bar's owed, the row hand-off a press on a marked row's mark arms acts as
// it always has. Nothing of the bar's is out: the reader unticks CORE-13 with a click, the bar
// staying over CORE-12's mark or going with the only mark, and within that click's frame and task
// a live update takes CORE-13's row away. The list's own hand-off hands the focus the row took
// with it to the Priority of the row before its place.
test.each([
  ["the bar stays over another mark", ["CORE-12", "CORE-13"], 1],
  ["the bar goes with the only mark", ["CORE-13"], 0],
] as const)(
  "with no landing of the bar's owed, a click on a marked row's mark whose row an update then takes away, where %s, hands the focus on by the list's own hand-off",
  (_bar, marked, after) => {
    const laidOut = layRowsOut("[data-row]", 9);
    const page = (count: number, listed: readonly string[]) => (
      <HandOffRows count={count} listed={listed} marked={marked} />
    );
    const view = render(page(marked.length, PLACED_KEYS));
    try {
      clickMark(rowBox("CORE-13"));
      view.rerender(page(after, PLACED_KEYS));
      expect(focusedName()).toBe("Select CORE-13");
      view.rerender(page(after, without(PLACED_KEYS, "CORE-13")));
      expect(focusedName()).toBe("Priority CORE-12");
    } finally {
      disarm();
      view.unmount();
      laidOut();
    }
  }
);

// A control remembered in a leaving row whose row goes while the bar stays owes nothing once the
// host's commit after that going still draws the bar. CORE-12 and CORE-13 are marked with the
// press's writes out; the reader ticks CORE-15, then unticks CORE-13 with a click, which leaves the
// focus on its checkbox, in a leaving row. Within that click's frame and task the writes take both
// rows away, and the bar stays. The list's own hand-off reads that commit first and holds its loss
// back, since the bar's going could still owe the landing there, as the project List takes its
// bar away a commit after the rows. The host's next commit, which prunes CORE-12 from the
// selection, still draws the bar over CORE-15: the bar forgets the checkbox and owes nothing, and
// the list's own hand-off, reading its loss again, hands the focus to the Priority of the row
// before the rows' place, not to the body. It reads that loss again only while the click's press
// is live: that press's end, a frame and a task after the click, ends the wait.
test("a click that unticks a leaving row's mark while another mark keeps the bar up, with that row then taken away and the bar still drawn in the host's next commit, lands by the list's own hand-off, not on the body", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const page = (count: number, listed: readonly string[], leaving: readonly string[] = []) => (
    <HandOffRows count={count} leaving={leaving} listed={listed} marked={UNTICK_PRESS} />
  );
  const view = render(page(2, PLACED_KEYS));
  let answered: () => void = () => undefined;
  try {
    pressInBar("Dismiss");
    answered = holdHandOff();
    view.rerender(page(2, PLACED_KEYS, UNTICK_PRESS));
    clickMark(rowBox("CORE-15"));
    expect(rowBox("CORE-15")).toBeChecked();
    view.rerender(page(3, PLACED_KEYS, UNTICK_PRESS));
    clickMark(rowBox("CORE-13"));
    expect(rowBox("CORE-13")).not.toBeChecked();
    view.rerender(page(2, PLACED_KEYS, UNTICK_PRESS));
    expect(focusedName()).toBe("Select CORE-13");
    // The writes land and take CORE-12 and CORE-13 away; CORE-12 is still selected.
    const kept = without(PLACED_KEYS, ...UNTICK_PRESS);
    view.rerender(page(2, kept));
    // The selection pruned, the bar over CORE-15 alone.
    view.rerender(page(1, kept));
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBeInTheDocument();
    expect(focusLanding()).toBe("Priority CORE-11");
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// A control remembered in a row that went while the bar stayed makes no later going owe a
// landing. As above, the reader unticked the row marked last with a click while the press's writes
// were out, and the focus stands on its checkbox in a leaving row; the writes take CORE-12 and
// CORE-13 away while another mark of the reader's keeps the bar up (the count stands for the marks
// still selected), and with no hand-off of the list's own the focus stays on the body. The host's
// next commit still draws the bar and forgets the checkbox. A later read then empties the bar with
// no act of the reader's, as a refetch does: nothing is owed, so nothing lands.
test("after the row of a remembered checkbox goes while the bar stays, a later going of the bar nobody caused owes nothing", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(untickList(2, PLACED_KEYS));
  let answered: () => void = () => undefined;
  try {
    answered = await untickWhileHeld(view);
    const kept = without(PLACED_KEYS, ...UNTICK_PRESS);
    view.rerender(untickList(1, kept));
    view.rerender(untickList(1, kept));
    expect(screen.getByRole("toolbar", { name: "Selected rows" })).toBeInTheDocument();
    expect(focusLanding()).toBe("the body");
    const moves = watchMoves();
    try {
      view.rerender(untickList(0, kept));
      expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
      expect(focusLanding()).toBe("the body");
      expect(moves.focused.map((move) => move.target.getAttribute("aria-label"))).toEqual([]);
    } finally {
      moves.restore();
    }
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// A press outside every row forgets the remembered control at its `pointerdown`, not a task after
// its `pointerup`. As above, the focus stands on the checkbox of the row marked last, in a leaving
// row; the reader presses a blank part of the page, which puts the focus on the body, their own
// choice. Within the task before that press would be read, the writes take both rows and the bar
// away together: nothing is owed, so nothing lands over the body, then or at the read.
test("after a click that unticks the row marked last while the press's writes are out, a press on a blank part of the page forgets its checkbox at once, so the rows and the bar going before that press's read land nothing", async () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(untickList(2, PLACED_KEYS));
  let answered: () => void = () => undefined;
  try {
    answered = await untickWhileHeld(view);
    const moves = watchMoves();
    try {
      act(() => {
        document.body.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true }));
        rowBox("CORE-13").blur();
        document.body.dispatchEvent(new PointerEvent("pointerup", { bubbles: true }));
        document.body.click();
      });
      expect(focusLanding()).toBe("the body");
      view.rerender(untickList(0, without(PLACED_KEYS, ...UNTICK_PRESS)));
      expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
      expect(focusLanding()).toBe("the body");
      await act(afterTask);
      expect(focusLanding()).toBe("the body");
      expect(moves.focused.map((move) => move.target.getAttribute("aria-label"))).toEqual([]);
    } finally {
      moves.restore();
    }
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

// Handed on covers the table's own landing on a row it is drawing (`drawRow`). The bar goes with
// the row marked last listed but not drawn, so the table is asked to draw it and land on its mark
// once drawn, as table-keys' pending focus does, and the focus waits on the list's own stop. The
// page then opens a dialog whose control takes the focus, in a commit before the table draws the
// row: that commit takes the table's landing back, so once the row is drawn the focus stays in the
// dialog.
test("a landing the table owes on a row it is drawing ends unmade, taken back, where the page moves the focus to a dialog's control before the row is drawn", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  // The row the table was asked to draw and land on, until that landing is taken back.
  const table: { pending: string | null } = { pending: null };
  const drawRow = (id: string) => {
    table.pending = id;
    return () => {
      if (table.pending === id) table.pending = null;
    };
  };
  const placed = (count: number, undrawn: readonly string[]) => (
    <PlacedList
      count={count}
      drawRow={drawRow}
      last="CORE-14"
      listed={PLACED_KEYS}
      undrawn={undrawn}
    />
  );
  const view = render(placed(1, ["CORE-14"]));
  const dialog = render(
    <div aria-label="Move to epic" role="dialog">
      <button aria-label="Confirm" type="button" />
    </div>
  );
  try {
    pressInBar("Dismiss");
    view.rerender(placed(0, ["CORE-14"]));
    expect(table.pending).toBe("CORE-14");
    expect(screen.getByRole("list", { name: "Rows" })).toHaveFocus();
    // The page's commit that opens the dialog, whose control takes the focus.
    act(() => screen.getByRole("button", { name: "Confirm" }).focus());
    view.rerender(placed(0, ["CORE-14"]));
    // The table draws CORE-14 and makes its landing, unless it was taken back.
    view.rerender(placed(0, []));
    const { pending } = table;
    if (pending !== null) act(() => rowBox(pending).focus({ preventScroll: true }));
    expect(focusLanding()).toBe("Confirm");
  } finally {
    disarm();
    dialog.unmount();
    view.unmount();
    laidOut();
  }
});
