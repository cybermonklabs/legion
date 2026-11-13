import { expect, mock, spyOn, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ControlRow, type ControlRowProps } from "./ControlRow";

/**
 * Tailwind's own definitions of the utilities the row's height rule is written in. happy-dom
 * applies a stylesheet it is given but loads none of the application's generated CSS, so the rule
 * is read from the computed style with these in the document: a line that stopped holding one
 * line, or a chips strip that started wrapping, computes something else here whatever class it
 * carries.
 */
const HEIGHT_RULE_CSS =
  ".flex{display:flex}.flex-col{flex-direction:column}.flex-wrap{flex-wrap:wrap}.flex-nowrap{flex-wrap:nowrap}.shrink-0{flex-shrink:0}.flex-1{flex:1 1 0%}.min-w-0{min-width:0}.gap-2{gap:0.5rem}.overflow-x-auto{overflow-x:auto}" +
  String.raw`.\[\&\>\*\]\:shrink-0>*{flex-shrink:0}`;
/** Tailwind's `sm:` utilities as they apply from 640 px up, which is where R1's two widths
 *  (1440 and 1280) are. Below it the first line and the page's own slot wrap, so that a phone's
 *  search box is a search box and a phone's own controls stay on the screen. */
const DESKTOP_CSS = String.raw`.sm\:flex-nowrap{flex-wrap:nowrap}.sm\:shrink-0{flex-shrink:0}`;
/** Tailwind's definitions of the size the row's own controls keep everywhere: the 44 px touch
 *  target of Clear filters and of the two scroll buttons. */
const TARGET_CSS = ".min-h-11{min-height:2.75rem}.h-11{height:2.75rem}.w-11{width:2.75rem}";
/** Tailwind's `md:` sizes as they apply from 768 px up, where a mouse is the pointer and the
 *  compact size gives that target up. */
const MOUSE_CSS = String.raw`.md\:min-h-8{min-height:2rem}.md\:h-8{height:2rem}.md\:w-8{width:2rem}`;

function Row(props: Partial<ControlRowProps>) {
  return (
    <ControlRow
      chips={<span data-testid="chips" />}
      display={<span data-testid="display" />}
      search={<span data-testid="search" />}
      trigger={<span data-testid="trigger" />}
      {...props}
    >
      <span data-testid="own" />
    </ControlRow>
  );
}

function row(): HTMLElement {
  const found = document.querySelector<HTMLElement>('[data-control="control-row"]');
  if (found === null) throw new Error("no control row is drawn");
  return found;
}

/** The row's lines, top to bottom: the first, and the chips line while there is a chip. */
function lines(): HTMLElement[] {
  return Array.from(row().children, (line) => line as HTMLElement);
}

/** The box the row drew around what a page handed one of its slots. */
function slotOf(testId: string): HTMLElement {
  const box = screen.getByTestId(testId).parentElement;
  if (box === null) throw new Error(`${testId} is drawn outside a slot of its own`);
  return box;
}

/** Puts Tailwind's rules for the row in the document for one test. */
function withCss(css: string): () => void {
  const style = document.createElement("style");
  style.textContent = css;
  document.head.appendChild(style);
  return () => style.remove();
}

test("the slots are drawn in the row's own order, whatever order the page names them in", () => {
  const view = render(<Row />);
  try {
    expect(row()).toHaveAttribute("data-control", "control-row");
    const drawn = Array.from(row().querySelectorAll("[data-testid]")).map((node) =>
      node.getAttribute("data-testid")
    );
    expect(drawn).toEqual(["search", "trigger", "own", "display", "chips"]);
    // The first line holds every slot but the chips, which have a line of their own under it.
    const [first, chips] = lines();
    for (const testId of ["search", "trigger", "own", "display"]) {
      expect(first.contains(screen.getByTestId(testId))).toBe(true);
    }
    expect(chips.contains(screen.getByTestId("chips"))).toBe(true);
  } finally {
    view.unmount();
  }
});

test("a slot the page leaves out is no empty box in the row", () => {
  const view = render(<ControlRow search={<span data-testid="search" />}>{null}</ControlRow>);
  try {
    expect(lines().length).toBe(1);
    expect(lines()[0].children.length).toBe(1);
    expect(lines()[0].children[0]).toBe(slotOf("search"));
  } finally {
    view.unmount();
  }
});

test("the chips line is drawn only while a chip is on, so a row with none is its first line alone", () => {
  const view = render(<Row chips={undefined} />);
  try {
    expect(lines().length).toBe(1);
    expect(screen.queryByTestId("chips")).toBeNull();
    view.rerender(<Row />);
    expect(lines().length).toBe(2);
    expect(lines()[1].contains(screen.getByTestId("chips"))).toBe(true);
    view.rerender(<Row chips={null} />);
    expect(lines().length).toBe(1);
  } finally {
    view.unmount();
  }
});

test("from the sm breakpoint up the first line is one line, and however many chips are on they take one more", () => {
  const removeCss = withCss(`${HEIGHT_RULE_CSS}${DESKTOP_CSS}`);
  const view = render(<Row />);
  try {
    // The row is a column of its lines, the row's 8 px gap apart.
    expect(getComputedStyle(row()).flexDirection).toBe("column");
    expect(getComputedStyle(row()).gap).toBe("0.5rem");
    // The first line is one line from `sm` up: no slot of it is ever pushed under another.
    const [first, chipsLine] = lines();
    expect(getComputedStyle(first).flexWrap).toBe("nowrap");
    expect(getComputedStyle(first).gap).toBe("0.5rem");

    // The chips never wrap: a chip that does not fit is scrolled to, so nine chips take the
    // same one line as one does (R1: one more line at most, however many narrowings are on).
    expect(getComputedStyle(chipsLine).flexWrap).toBe("nowrap");
    const strip = slotOf("chips");
    expect(getComputedStyle(strip).flexWrap).toBe("nowrap");
    expect(getComputedStyle(strip).overflowX).toBe("auto");
    // The strip takes the line's room the rest of it leaves, and gives way rather than push
    // Clear filters past the edge.
    expect(getComputedStyle(strip).flexGrow).toBe("1");
    expect(getComputedStyle(strip).minWidth).toBe("0");
    // A chip the strip has no room for is scrolled to, never squeezed: a page's own chip that
    // could shrink would be drawn narrower than its text, under the next chip.
    expect(getComputedStyle(screen.getByTestId("chips")).flexShrink).toBe("0");
    // And no box in the row may take another line at all.
    const wrapping = Array.from(row().querySelectorAll("*")).filter(
      (node) => getComputedStyle(node).flexWrap === "wrap"
    );
    expect(wrapping).toEqual([]);

    // The search box is the one slot that gives way where the window is narrow, as it does on
    // every page today; it still lays its own nodes out in one line.
    expect(getComputedStyle(slotOf("search")).flexGrow).toBe("1");
    expect(getComputedStyle(slotOf("search")).flexWrap).toBe("nowrap");
    for (const testId of ["trigger", "own", "display"]) {
      expect(getComputedStyle(slotOf(testId)).flexShrink).toBe("0");
      expect(getComputedStyle(slotOf(testId)).flexWrap).toBe("nowrap");
    }
  } finally {
    view.unmount();
    removeCss();
  }
});

test("below the phone breakpoint the first line and the page's own controls wrap onto the screen", () => {
  // The same utilities with no `sm:` rule applied, which is a window narrower than 640 px.
  const removeCss = withCss(HEIGHT_RULE_CSS);
  const view = render(<Row />);
  try {
    // A 390 px phone cannot hold the search box, the trigger, the page's own controls and the
    // display menu on one line, so the first line wraps rather than running past the right edge
    // with a control the reader cannot reach (the Artifacts tab's staged upload). That wrap is
    // there before any narrowing, since no chip is in that line.
    const [first, chipsLine] = lines();
    expect(getComputedStyle(first).flexWrap).toBe("wrap");
    expect(getComputedStyle(slotOf("own")).flexWrap).toBe("wrap");
    expect(getComputedStyle(slotOf("own")).flexShrink).not.toBe("0");
    expect(first.contains(screen.getByTestId("chips"))).toBe(false);
    // The chips still take one line, and still never wrap: that line, a 44 px chip and the
    // row's 8 px gap, is the 52 px R1 allows, and nothing else moves.
    expect(getComputedStyle(chipsLine).flexWrap).toBe("nowrap");
    const strip = slotOf("chips");
    expect(getComputedStyle(strip).flexWrap).toBe("nowrap");
    expect(getComputedStyle(strip).overflowX).toBe("auto");
  } finally {
    view.unmount();
    removeCss();
  }
});

test("while a chip is on, Clear filters is drawn from onClear at the end of the chips line, outside the strip", async () => {
  const user = userEvent.setup();
  const onClear = mock(() => {});
  const view = render(<Row onClear={onClear} />);
  try {
    const clear = screen.getByRole("button", { name: "Clear filters" });
    const [first, chipsLine] = lines();
    expect(first.contains(clear)).toBe(false);
    expect(chipsLine.lastElementChild).toBe(clear);
    // Outside the strip and the box its scroll buttons are laid over, so no number of chips
    // scrolls it away or covers it, and after it in the reading order.
    const strip = slotOf("chips");
    expect(strip.contains(clear)).toBe(false);
    expect(strip.parentElement?.contains(clear)).toBe(false);
    expect(strip.compareDocumentPosition(clear) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(clear).not.toHaveAttribute("data-control");
    expect(clear).toHaveAttribute("type", "button");
    await user.click(clear);
    expect(onClear).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("with no chip on, Clear filters sits in the first line right after the trigger, and adds no line", () => {
  const view = render(<Row chips={undefined} onClear={() => {}} />);
  try {
    expect(lines().length).toBe(1);
    const clear = screen.getByRole("button", { name: "Clear filters" });
    expect(Array.from(slotOf("trigger").children)).toEqual([screen.getByTestId("trigger"), clear]);
  } finally {
    view.unmount();
  }
});

test("the row draws no Clear filters without onClear, chips or no chips", () => {
  const view = render(<Row />);
  try {
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
    view.rerender(<Row chips={undefined} />);
    expect(screen.queryByRole("button", { name: "Clear filters" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("Clear filters keeps the 44 px target, and gives it up from md only at the compact size", () => {
  const view = render(<Row onClear={() => {}} />);
  // Its height under `css`: a window below `md` (the target alone), or from it (with `md:`).
  const minHeight = (css: string) => {
    const removeCss = withCss(css);
    try {
      return getComputedStyle(screen.getByRole("button", { name: "Clear filters" })).minHeight;
    } finally {
      removeCss();
    }
  };
  try {
    // happy-dom computes these lengths in px: 2.75rem is the 44 px target, 2rem 32 px.
    expect(minHeight(TARGET_CSS)).toBe("44px");
    expect(minHeight(`${TARGET_CSS}${MOUSE_CSS}`)).toBe("44px");
    view.rerender(<Row onClear={() => {}} size="compact" />);
    expect(minHeight(TARGET_CSS)).toBe("44px");
    expect(minHeight(`${TARGET_CSS}${MOUSE_CSS}`)).toBe("32px");
  } finally {
    view.unmount();
  }
});

test("no scroll button is drawn while nothing is out of view on either side", () => {
  // happy-dom lays nothing out, so the strip never overflows here: the buttons are hidden, not
  // drawn disabled.
  const view = render(<Row onClear={() => {}} />);
  try {
    expect(screen.queryByRole("button", { name: "Scroll filters left" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Scroll filters right" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("a scroll button keeps the 44 px target, and gives it up from md only at the compact size", () => {
  const view = render(<Row />);
  // Its box under `css`: a window below `md` (the target alone), or from it (with `md:`).
  const box = (css: string) => {
    const removeCss = withCss(css);
    try {
      const style = getComputedStyle(screen.getByRole("button", { name: "Scroll filters right" }));
      return { height: style.height, width: style.width };
    } finally {
      removeCss();
    }
  };
  try {
    // happy-dom lays nothing out, so the strip is told it holds more than it shows, and a scroll
    // of it reads that again: the right button is drawn.
    const strip = slotOf("chips");
    Object.defineProperty(strip, "scrollWidth", { configurable: true, value: 1000 });
    act(() => {
      strip.dispatchEvent(new Event("scroll"));
    });
    const target = { height: "44px", width: "44px" };
    expect(box(TARGET_CSS)).toEqual(target);
    expect(box(`${TARGET_CSS}${MOUSE_CSS}`)).toEqual(target);
    view.rerender(<Row size="compact" />);
    expect(box(TARGET_CSS)).toEqual(target);
    expect(box(`${TARGET_CSS}${MOUSE_CSS}`)).toEqual({ height: "32px", width: "32px" });
  } finally {
    view.unmount();
  }
});

test("a chip that takes the focus is scrolled wholly into view inside the strip", async () => {
  const user = userEvent.setup();
  const view = render(
    <ControlRow
      chips={
        <button data-testid="chip" type="button">
          Chip
        </button>
      }
    />
  );
  try {
    const chip = screen.getByTestId("chip");
    const scrolled = spyOn(chip, "scrollIntoView").mockImplementation(() => {});
    await user.tab();
    expect(document.activeElement).toBe(chip);
    expect(scrolled).toHaveBeenCalledWith({ block: "nearest", inline: "nearest" });
  } finally {
    view.unmount();
  }
});
