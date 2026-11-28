import { expect, mock, spyOn, test } from "bun:test";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { layOutLine, withCss } from "../__tests__/chips-line";
import { MOUSE_CSS, SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { afterFrameAndTasks } from "../__tests__/frames";
import { installResizeObserver, layOutPanel, stacked } from "../__tests__/scroll-box";
import { Chip } from "./Chip";
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
  String.raw`.\[\&\>\*\]\:shrink-0>*{flex-shrink:0}.\@container{container-type:inline-size}`;
/** The row's own container variants (`@min-[40rem]:`) as they apply where the row's box is 40rem
 *  (640 px) wide or more. In a narrower row - a phone's, or the Inbox drawer's at any window -
 *  the first line and the page's own slot wrap, so that a search box is a search box and the
 *  page's own controls stay inside the row. */
const WIDE_ROW_CSS = String.raw`.\@min-\[40rem\]\:flex-nowrap{flex-wrap:nowrap}.\@min-\[40rem\]\:shrink-0{flex-shrink:0}`;

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

test("in a row 40rem wide or more the first line is one line, and however many chips are on they take one more", () => {
  const removeCss = withCss(`${HEIGHT_RULE_CSS}${WIDE_ROW_CSS}`);
  const view = render(<Row />);
  try {
    // The row is a column of its lines, the row's 8 px gap apart.
    expect(getComputedStyle(row()).flexDirection).toBe("column");
    expect(getComputedStyle(row()).gap).toBe("0.5rem");
    // The row is the container its first line and slots read, so they switch on the row's own
    // width and not the window's: the Inbox drawer's row is 415 px wide in a 1440 px window.
    expect(getComputedStyle(row()).getPropertyValue("container-type")).toBe("inline-size");
    // The first line is one line in a row that wide: no slot of it is ever pushed under another.
    const [first, chipsLine] = lines();
    expect(getComputedStyle(first).flexWrap).toBe("nowrap");
    expect(getComputedStyle(first).gap).toBe("0.5rem");

    // The chips never wrap: a chip that does not fit is listed behind `+N more`, so nine chips
    // take the same one line as one does (R1: one more line at most, however many narrowings are
    // on).
    expect(getComputedStyle(chipsLine).flexWrap).toBe("nowrap");
    const strip = slotOf("chips");
    expect(getComputedStyle(strip).flexWrap).toBe("nowrap");
    // And nothing in the line scrolls sideways.
    expect(getComputedStyle(strip).overflowX).not.toMatch(/auto|scroll/);
    // The box the chips and `+N more` share takes the line's room the rest of it leaves, and
    // gives way rather than push Clear filters past the edge, as the strip in it does.
    const box = strip.parentElement;
    if (box === null) throw new Error("the strip is drawn outside a box of its own");
    expect(getComputedStyle(box).flexGrow).toBe("1");
    expect(getComputedStyle(box).minWidth).toBe("0");
    expect(getComputedStyle(strip).minWidth).toBe("0");
    // A chip the line draws is never squeezed: a page's own chip that could shrink would be
    // drawn narrower than its text, under the next chip.
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

test("in a row narrower than 40rem the first line and the page's own controls wrap inside it", () => {
  // The same utilities with no container variant applied, which is a row narrower than 40rem.
  const removeCss = withCss(HEIGHT_RULE_CSS);
  const view = render(<Row />);
  try {
    // A 390 px phone, or the Inbox drawer's 415 px row, cannot hold the search box, the trigger,
    // the page's own controls and the display menu on one line, so the first line wraps rather
    // than running past the row's right edge with a control the reader cannot reach (the
    // Artifacts tab's staged upload). That wrap is there before any narrowing, since no chip is
    // in that line.
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
    expect(getComputedStyle(strip).overflowX).not.toMatch(/auto|scroll/);
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
    // Outside the strip and the box the chips share with `+N more`, so no number of chips pushes
    // it out of view, and after them in the reading order.
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
  // Its height under `css`: a window below `md` (`SIZE_CSS` alone), or from it (with `md:`).
  const minHeight = (css: string) =>
    sizeUnder(screen.getByRole("button", { name: "Clear filters" }), css).minHeight;
  try {
    // happy-dom computes these lengths in px: 2.75rem is the 44 px target, 2rem 32 px.
    expect(minHeight(SIZE_CSS)).toBe("44px");
    expect(minHeight(`${SIZE_CSS}${MOUSE_CSS}`)).toBe("44px");
    view.rerender(<Row onClear={() => {}} size="compact" />);
    expect(minHeight(SIZE_CSS)).toBe("44px");
    expect(minHeight(`${SIZE_CSS}${MOUSE_CSS}`)).toBe("32px");
  } finally {
    view.unmount();
  }
});

/**
 * A row whose chips are buttons that take themselves off when pressed, as every page's chips do,
 * beside a filter trigger and a Clear filters that takes every chip off at once. Given `held`, a
 * pressed chip is named as going at once - the row draws again with the chip still in it - and is
 * taken off only when the promise `held` hands it settles, as on a page whose removal waits on its
 * navigation.
 */
function RemovableChips({ held, names }: { held?: () => Promise<void>; names: readonly string[] }) {
  const [on, setOn] = useState(names);
  const [going, setGoing] = useState<string>();
  const takeOff = (name: string) => setOn((chips) => chips.filter((chip) => chip !== name));
  return (
    <ControlRow
      chips={
        on.length === 0
          ? undefined
          : on.map((name) => (
              <button
                key={name}
                onClick={() => {
                  if (held === undefined) {
                    takeOff(name);
                    return;
                  }
                  setGoing(name);
                  void held().then(() => takeOff(name));
                }}
                type="button"
              >
                {name}
              </button>
            ))
      }
      onClear={() => setOn([])}
      search={<input aria-label="Search" type="search" />}
      trigger={
        <button data-control="panel-trigger" type="button">
          Filter
        </button>
      }
    >
      {going === undefined ? null : <span>{going} is going</span>}
    </ControlRow>
  );
}

/** The chip removals `RemovableChips` holds, each committed when the test releases it. */
function heldRemovals(): { hold: () => Promise<void>; release: () => Promise<void> } {
  const waiting: (() => void)[] = [];
  return {
    hold: () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      waiting.push(resolve);
      return promise;
    },
    release: () =>
      act(async () => {
        for (const resolve of waiting.splice(0)) resolve();
        await Promise.resolve();
      }),
  };
}

test("a chip taken off hands the focus to the chip now at its place", async () => {
  const user = userEvent.setup();
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Beta" }));
    expect(screen.queryByRole("button", { name: "Beta" })).toBeNull();
    // Not the page's body, from which a keyboard reader would start again at its first link.
    expect(screen.getByRole("button", { name: "Gamma" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("the last chip taken off hands the focus to the chip before it", async () => {
  const user = userEvent.setup();
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Gamma" }));
    expect(screen.queryByRole("button", { name: "Gamma" })).toBeNull();
    expect(screen.getByRole("button", { name: "Beta" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("the only chip taken off hands the focus to the filter trigger", async () => {
  const user = userEvent.setup();
  const view = render(<RemovableChips names={["Alpha"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Alpha" }));
    // The chips line went with its last chip, strip and all.
    expect(lines().length).toBe(1);
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Clear filters hands the focus to the filter trigger", async () => {
  const user = userEvent.setup();
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(lines().length).toBe(1);
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Delete on a focused chip takes it off, as Enter and Space do, and hands the focus on", async () => {
  const user = userEvent.setup();
  const view = render(<RemovableChips names={["Alpha", "Beta"]} />);
  try {
    act(() => screen.getByRole("button", { name: "Alpha" }).focus());
    await user.keyboard("{Delete}");
    expect(screen.queryByRole("button", { name: "Alpha" })).toBeNull();
    expect(screen.getByRole("button", { name: "Beta" })).toHaveFocus();
    await user.keyboard("{Delete}");
    expect(lines().length).toBe(1);
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("Delete on a link a page drew inside its chip beside the chip's own control takes nothing off and follows nothing", async () => {
  const user = userEvent.setup();
  const cleared = mock(() => {});
  const followed = mock(() => {});
  // The Inbox's agent chip: a box holding the shared chip that clears the filter and, after it,
  // the link to the agent's live view.
  const view = render(
    <ControlRow
      chips={
        <span>
          <Chip aria-label="Clear agent filter" onClick={cleared} removable selected>
            Asks from Planner
          </Chip>
          <a
            href="/agents/planner/live"
            onClick={(event) => {
              event.preventDefault();
              followed();
            }}
          >
            Open live conversation
          </a>
        </span>
      }
      trigger={
        <button data-control="panel-trigger" type="button">
          Filter
        </button>
      }
    />
  );
  try {
    const live = screen.getByRole("link", { name: "Open live conversation" });
    act(() => live.focus());
    await user.keyboard("{Delete}");
    expect(followed).not.toHaveBeenCalled();
    expect(cleared).not.toHaveBeenCalled();
    expect(live).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a removal that commits later hands the focus on then, whatever held it meanwhile, and moves nothing once the reader has moved it elsewhere", async () => {
  const user = userEvent.setup();
  const removals = heldRemovals();
  const view = render(
    <RemovableChips held={removals.hold} names={["Alpha", "Beta", "Gamma", "Delta"]} />
  );
  try {
    // The row draws again at once with the chip still in it: the hand-off waits.
    await user.click(screen.getByRole("button", { name: "Alpha" }));
    expect(screen.getByText("Alpha is going")).toBeDefined();
    expect(screen.getByRole("button", { name: "Alpha" })).toHaveFocus();
    await removals.release();
    expect(screen.queryByRole("button", { name: "Alpha" })).toBeNull();
    expect(screen.getByRole("button", { name: "Beta" })).toHaveFocus();
    // A press that leaves the focus on the body, as a pointer's click in Safari does: the
    // hand-off still waits for the removal rather than focus the chip that is going.
    act(() => screen.getByRole("button", { name: "Beta" }).blur());
    act(() => screen.getByRole("button", { name: "Gamma" }).click());
    expect(screen.getByText("Gamma is going")).toBeDefined();
    expect(document.activeElement).toBe(document.body);
    await removals.release();
    expect(screen.queryByRole("button", { name: "Gamma" })).toBeNull();
    expect(screen.getByRole("button", { name: "Delta" })).toHaveFocus();
    // The reader moves to the search box before Beta's removal commits, and the focus stays.
    await user.click(screen.getByRole("button", { name: "Beta" }));
    const search = screen.getByRole("searchbox", { name: "Search" });
    await user.click(search);
    await removals.release();
    expect(screen.queryByRole("button", { name: "Beta" })).toBeNull();
    expect(search).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** The words of each chip the line draws, in its order. */
function lineChips(): string[] {
  const strip = document.querySelector("[data-chip-strip]");
  return Array.from(strip?.children ?? [], (chip) => chip.textContent ?? "");
}

/** The words of each chip the open panel of `+N more` lists, in its order. */
function panelChips(name: string): string[] {
  const panel = screen.getByRole("dialog", { name });
  return within(panel)
    .getAllByRole("button")
    .map((chip) => chip.textContent ?? "");
}

/** Four chips of 100 px. A 300 px line holds two of them and `+2 more`, which take 100 + 8 +
 *  100 + 8 + 60 = 276 px; a third chip would take it to 384. */
const FOUR = { Alpha: 100, Beta: 100, Delta: 100, Gamma: 100 };

test("chips past the line's room are drawn behind one +N more, named for what it opens", () => {
  const restore = layOutLine({ width: 300 }, FOUR);
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    const more = screen.getByRole("button", { name: "2 more filters" });
    expect(more).toHaveTextContent("+2 more");
    expect(more).toHaveAttribute("data-control", "chip");
    expect(more).toHaveAttribute("aria-haspopup", "dialog");
    expect(more).toHaveAttribute("aria-expanded", "false");
    // At the end of the chips, outside the strip, and before Clear filters.
    expect(document.querySelector("[data-chip-strip]")?.nextElementSibling).toBe(more);
    expect(lines()[1].lastElementChild).toBe(screen.getByRole("button", { name: "Clear filters" }));
    // The chips left out are drawn nowhere until it opens.
    expect(screen.queryByRole("button", { name: "Gamma" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Delta" })).toBeNull();
  } finally {
    view.unmount();
    restore();
  }
});

test("+N more opens a panel of the chips left out, with the focus on its first, and each chip is drawn once", async () => {
  const user = userEvent.setup();
  const restore = layOutLine({ width: 300 }, FOUR);
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    const more = screen.getByRole("button", { name: "2 more filters" });
    await user.click(more);
    expect(panelChips("2 more filters")).toEqual(["Gamma", "Delta"]);
    expect(more).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("button", { name: "Gamma" })).toHaveFocus();
    // In the line or in the panel, never both.
    for (const name of ["Alpha", "Beta", "Gamma", "Delta"]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
  } finally {
    view.unmount();
    restore();
  }
});

test("Escape in the panel, and Tab past its last chip, close it and hand the focus back to +N more", async () => {
  const user = userEvent.setup();
  const restore = layOutLine({ width: 300 }, FOUR);
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    const more = screen.getByRole("button", { name: "2 more filters" });
    await user.click(more);
    expect(screen.getByRole("button", { name: "Gamma" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(more).toHaveFocus();
    expect(more).toHaveAttribute("aria-expanded", "false");
    // Opened again from the keyboard, and left by Tab past its last chip.
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Gamma" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Delta" })).toHaveFocus();
    await user.tab();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(more).toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

// Here no engine scrolls the panel itself, as Firefox scrolls no chip already partly in sight.
test("a chip the focus reaches in the panel of +N more stands whole in the panel's box, which scrolls the least that shows it, and nothing else scrolls", async () => {
  const user = userEvent.setup();
  const names = ["Alpha", "Beta", "Gamma", "Delta", "Epsilon", "Zeta", "Eta"];
  const restore = layOutLine({ width: 300 }, Object.fromEntries(names.map((name) => [name, 100])));
  const view = render(<RemovableChips names={names} />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    await user.click(screen.getByRole("button", { name: "5 more filters" }));
    const panel = screen.getByRole("dialog", { name: "5 more filters" });
    // Five chips hold 8 + 5 × 52 px.
    const chips = within(panel).getAllByRole("button");
    layOutPanel(panel, 8 + 52 * chips.length, stacked(chips));
    expect(screen.getByRole("button", { name: "Gamma" })).toHaveFocus();
    // The chip each key puts the focus on, and how far the panel has scrolled then: the panel
    // shows 0-198 px of what it holds, and the fourth chip, Zeta, stands at 164-208.
    const walked: [string, number][] = [];
    for (const shift of [false, false, false, false, true, true, true, true]) {
      await user.tab({ shift });
      walked.push([document.activeElement?.textContent ?? "nothing", panel.scrollTop]);
    }
    expect(walked).toEqual([
      ["Delta", 0],
      ["Epsilon", 0],
      ["Zeta", 10],
      ["Eta", 62],
      // Already whole, so nothing scrolls.
      ["Zeta", 62],
      ["Epsilon", 62],
      // Back up, each chip stands at the box's top edge, so the first leaves the panel's 8 px of
      // padding above it out of sight.
      ["Delta", 60],
      ["Gamma", 8],
    ]);
    expect(scrolls).not.toHaveBeenCalled();
  } finally {
    scrolls.mockRestore();
    view.unmount();
    restore();
  }
});

test("a chip taken off in the panel goes, and the room it frees takes the next chip into the line as +N counts down", async () => {
  const user = userEvent.setup();
  // Two 100 px chips and `+3 more` take 276 px of a 300 px line, and the 200 px chip after them
  // does not fit. With it gone, the 16 px chip after it does: 100 + 8 + 100 + 8 + 16 + 8 + 60 is
  // 300 px.
  const restore = layOutLine(
    { width: 300 },
    { Alpha: 100, Beta: 100, Delta: 100, Gamma: 16, Wide: 200 }
  );
  const view = render(<RemovableChips names={["Alpha", "Beta", "Wide", "Gamma", "Delta"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "3 more filters" }));
    expect(screen.getByRole("button", { name: "Wide" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Wide" })).toBeNull();
    expect(lineChips()).toEqual(["Alpha", "Beta", "Gamma"]);
    expect(screen.getByRole("button", { name: "1 more filter" })).toHaveTextContent("+1 more");
    expect(panelChips("1 more filter")).toEqual(["Delta"]);
    // The panel stands, so the focus stays in it, on the chip now at the place of the one taken
    // off.
    expect(screen.getByRole("button", { name: "Delta" })).toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

test("the panel's last chip taken off closes the panel, and the focus goes to the line's last chip", async () => {
  const user = userEvent.setup();
  // Two of three 100 px chips and `+1 more` fit a 300 px line; the two alone need no `+N more`.
  const restore = layOutLine({ width: 300 }, { Alpha: 100, Beta: 100, Gamma: 100 });
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma"]} />);
  try {
    await user.click(screen.getByRole("button", { name: "1 more filter" }));
    expect(screen.getByRole("button", { name: "Gamma" })).toHaveFocus();
    // Delete takes a chip off in the panel as it does in the line.
    await user.keyboard("{Delete}");
    expect(screen.queryByRole("button", { name: "Gamma" })).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("button", { name: /more filters?$/ })).toBeNull();
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    // Not the page's body: the chip before the place the panel's chips stood at.
    expect(screen.getByRole("button", { name: "Beta" })).toHaveFocus();
  } finally {
    view.unmount();
    restore();
  }
});

test("a line that widens takes back the chips that now fit, and one that narrows hands them to +N more again", () => {
  const sizes = installResizeObserver();
  const room = { width: 300 };
  const restore = layOutLine(room, FOUR);
  const view = render(<RemovableChips names={["Alpha", "Beta", "Gamma", "Delta"]} />);
  try {
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    // Four 100 px chips and their three gaps take 424 px: a 440 px line holds them all, and
    // needs no `+N more`.
    room.width = 440;
    sizes.resize();
    expect(lineChips()).toEqual(["Alpha", "Beta", "Gamma", "Delta"]);
    expect(screen.queryByRole("button", { name: /more filters?$/ })).toBeNull();
    room.width = 300;
    sizes.resize();
    expect(lineChips()).toEqual(["Alpha", "Beta"]);
    expect(screen.getByRole("button", { name: "2 more filters" })).toHaveTextContent("+2 more");
  } finally {
    view.unmount();
    restore();
    sizes.restore();
  }
});

test("a first chip wider than the whole line is still drawn in it, before +N more", () => {
  const long = "Label: a name that runs past the line";
  const restore = layOutLine({ width: 300 }, { Beta: 50, [long]: 400 });
  const view = render(<RemovableChips names={[long, "Beta"]} />);
  try {
    expect(lineChips()).toEqual([long]);
    expect(screen.getByRole("button", { name: "1 more filter" })).toHaveTextContent("+1 more");
  } finally {
    view.unmount();
    restore();
  }
});

test("each chip of a fragment is a chip of the line, as the Inbox hands its agent chip before the rest", () => {
  const restore = layOutLine(
    { width: 300 },
    { "Asks from Planner": 100, "Type: Question": 100, "Urgency: High": 100 }
  );
  const view = render(
    <ControlRow
      chips={
        <>
          <span>
            <Chip aria-label="Clear agent filter" onClick={() => {}}>
              Asks from Planner
            </Chip>
          </span>
          {[
            <Chip key="type" onClick={() => {}}>
              Type: Question
            </Chip>,
            <Chip key="urgency" onClick={() => {}}>
              Urgency: High
            </Chip>,
          ]}
        </>
      }
    />
  );
  try {
    expect(lineChips()).toEqual(["Asks from Planner", "Type: Question"]);
    expect(screen.getByRole("button", { name: "1 more filter" })).toHaveTextContent("+1 more");
  } finally {
    view.unmount();
    restore();
  }
});

/** A row whose page slot holds controls that each take themselves away when pressed, as the
 *  Inbox's Mark all read does once nothing is left unread, beside a filter trigger. */
function SelfRemovingControls({ names }: { names: readonly string[] }) {
  const [on, setOn] = useState(names);
  return (
    <ControlRow
      search={<input aria-label="Search" type="search" />}
      trigger={
        <button data-control="panel-trigger" type="button">
          Filter
        </button>
      }
    >
      {on.length === 0
        ? undefined
        : on.map((name) => (
            <button
              key={name}
              onClick={() => setOn((left) => left.filter((other) => other !== name))}
              type="button"
            >
              {name}
            </button>
          ))}
    </ControlRow>
  );
}

test("a page's own control that takes itself away hands the focus to the next control in its slot, then the one before it, then the filter trigger", async () => {
  const user = userEvent.setup();
  const view = render(
    <SelfRemovingControls names={["Mark all read", "Blocked on you", "Snooze all"]} />
  );
  try {
    act(() => screen.getByRole("button", { name: "Mark all read" }).focus());
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Mark all read" })).toBeNull();
    // Not the page's body, from which a keyboard reader would start again at its first link.
    expect(screen.getByRole("button", { name: "Blocked on you" })).toHaveFocus();
    // The slot's last control has no next one: the one before it.
    act(() => screen.getByRole("button", { name: "Snooze all" }).focus());
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Blocked on you" })).toHaveFocus();
    // Its only control: the slot goes with it, and the focus goes to the trigger.
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Blocked on you" })).toBeNull();
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** A row whose page slot holds a "New group name" box and, after it, a Delete group that takes
 *  itself away when pressed, as the delivery editor draws them for its only group, beside a
 *  filter trigger; with `added`, an Add group button before the box. */
function GroupEditorRow({ added }: { added: boolean }) {
  const [deleted, setDeleted] = useState(false);
  return (
    <ControlRow
      trigger={
        <button data-control="panel-trigger" type="button">
          Filter
        </button>
      }
    >
      {added ? <button type="button">Add group</button> : null}
      <input aria-label="New group name" type="text" />
      {deleted ? null : (
        <button onClick={() => setDeleted(true)} type="button">
          Delete group
        </button>
      )}
    </ControlRow>
  );
}

// A hand-off passes over every control that takes typed text: the reader never chose such a
// field, and their next key would be typed into it. So Delete group's going hands the focus over
// the "New group name" box before it to the control before that, and to the filter trigger
// where only the box stands before it.
test("a page's own control that takes itself away hands the focus past a text box before it, to the control before that, else to the filter trigger", async () => {
  const user = userEvent.setup();
  // Where the focus went once Delete group had gone, with an Add group before the box and with
  // none.
  const landed: Record<string, string | null> = {};
  for (const added of [true, false]) {
    const view = render(<GroupEditorRow added={added} />);
    try {
      act(() => screen.getByRole("button", { name: "Delete group" }).focus());
      await user.keyboard("{Enter}");
      expect(screen.queryByRole("button", { name: "Delete group" })).toBeNull();
      const focused = document.activeElement;
      landed[added ? "with Add group" : "the box alone"] =
        focused?.getAttribute("aria-label") ?? focused?.textContent ?? null;
    } finally {
      view.unmount();
    }
  }
  expect(landed).toEqual({ "the box alone": "Filter", "with Add group": "Add group" });
});

/** The shell's main content holding a row whose page slot draws exactly `names`, as a live update
 *  leaves them. */
function LivePage({ names }: { names: readonly string[] }) {
  return (
    <main>
      <ControlRow
        trigger={
          <button data-control="panel-trigger" type="button">
            Filter
          </button>
        }
      >
        {names.map((name) => (
          <button key={name} type="button">
            {name}
          </button>
        ))}
      </ControlRow>
    </main>
  );
}

// INFRA-3135's review round 1, R10: a live update - the last unread item read in another tab -
// takes Mark all read away while it holds the focus, after the reader scrolled it out of their
// sight. The slot's hand-off hands the focus to no control - a live update is no press of the
// reader's - so neither Blocked on you takes it nor the page is scrolled back to the row.
test("a page's own control the reader scrolled out of sight goes with a live update and hands the focus to nothing", async () => {
  const view = render(<LivePage names={["Mark all read", "Blocked on you"]} />);
  try {
    const mark = screen.getByRole("button", { name: "Mark all read" });
    act(() => mark.focus());
    // The page scrolled the row 2,000 px above the window's top edge.
    mark.getBoundingClientRect = () => new DOMRect(0, -2_000, 120, 44);
    act(() => {
      document.dispatchEvent(new Event("scroll"));
    });
    view.rerender(<LivePage names={["Blocked on you"]} />);
    await afterFrameAndTasks();
    expect(screen.queryByRole("button", { name: "Mark all read" })).toBeNull();
    expect(screen.getByRole("button", { name: "Blocked on you" })).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
  } finally {
    view.unmount();
  }
});

/** A row whose page slot draws a modal dialog, as the Delivery page draws its Session groups
 *  dialog there: a Close button and a link, then a Groups section holding the only group's Delete
 *  group, which takes the section away when pressed. */
function DialogInSlotRow() {
  const [deleted, setDeleted] = useState(false);
  return (
    <ControlRow
      trigger={
        <button data-control="panel-trigger" type="button">
          Filter · 0
        </button>
      }
    >
      <aside aria-label="Session groups" aria-modal="true" role="dialog">
        <button type="button">Close session groups</button>
        <a href="/agents/s3/live">Open live conversation with Reviewer</a>
        {deleted ? null : (
          <section>
            <button onClick={() => setDeleted(true)} type="button">
              Delete group
            </button>
          </section>
        )}
      </aside>
    </ControlRow>
  );
}

// A control of a dialog drawn in a slot is the dialog's, not the row's. The reader has scrolled
// the dialog so that every control before Delete group is out of sight, and presses Delete group,
// which takes the Groups section with it. The row hands that focus to none of its own controls,
// so the filter trigger, in sight, does not take it: nothing moves, and nothing scrolls.
test("a control of a dialog drawn in a slot hands its focus to no control of the row", async () => {
  const user = userEvent.setup();
  const view = render(<DialogInSlotRow />);
  const scrolls = spyOn(Element.prototype, "scrollIntoView");
  try {
    const dialog = screen.getByRole("dialog", { name: "Session groups" });
    const deleteGroup = screen.getByRole("button", { name: "Delete group" });
    // Scrolled 500 px above the window's top edge: happy-dom lays nothing out.
    for (const control of dialog.querySelectorAll("a, button")) {
      if (control !== deleteGroup) {
        control.getBoundingClientRect = () => new DOMRect(0, -500, 200, 40);
      }
    }
    act(() => deleteGroup.focus());
    await user.keyboard("{Enter}");
    await afterFrameAndTasks();
    expect(screen.queryByRole("button", { name: "Delete group" })).toBeNull();
    const focused = document.activeElement;
    // Where the focus went once Delete group had gone, and how many scrolls the page was asked for.
    const landed = {
      focus:
        focused === null || focused === document.body
          ? "the page"
          : (focused.getAttribute("aria-label") ?? focused.textContent),
      scrolls: scrolls.mock.calls.length,
    };
    expect(landed).toEqual({ focus: "the page", scrolls: 0 });
  } finally {
    scrolls.mockRestore();
    view.unmount();
  }
});
