import { expect, mock, spyOn, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { afterRescue, RescuedMain } from "../__tests__/focus-rescue";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { appKeymap, type KeymapScope, useKeymapScope } from "../features/shell/keymap";
import { ControlRow, type ControlRowProps } from "./ControlRow";
import { DisplayMenu } from "./DisplayMenu";
import { PanelTrigger } from "./PanelTrigger";

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
  // The Inbox's agent chip: a box holding the link that clears the filter and, after it, the
  // link to the agent's live view.
  const view = render(
    <ControlRow
      chips={
        <span>
          <a
            href="/"
            onClick={(event) => {
              event.preventDefault();
              cleared();
            }}
          >
            Clear agent filter
          </a>
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

/** The shell's main content holding a row whose page slot draws exactly `names`, as a live update
 *  leaves them, with the page's focus rescue mounted on it as `app.tsx` mounts it. */
function LivePage({ names }: { names: readonly string[] }) {
  return (
    <RescuedMain>
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
    </RescuedMain>
  );
}

// INFRA-3135's review round 1, R10: a live update - the last unread item read in another tab -
// takes Mark all read away while it holds the focus, after the reader scrolled it out of their
// sight. The slot's hand-off keeps the rescue's rule, read from the rescue's own record of what
// the reader can see: it hands the focus to no control, so neither Blocked on you takes it nor
// the page is scrolled back to the row.
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
    await afterRescue();
    expect(screen.queryByRole("button", { name: "Mark all read" })).toBeNull();
    expect(screen.getByRole("button", { name: "Blocked on you" })).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
  } finally {
    view.unmount();
  }
});

test("a press on a scroll button takes no focus, so a chip that held it keeps it", async () => {
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
    // happy-dom lays nothing out, so the strip is told it holds more than it shows, and a scroll
    // of it reads that again: the right button is drawn.
    const chip = screen.getByTestId("chip");
    const strip = slotOf("chip");
    Object.defineProperty(strip, "scrollWidth", { configurable: true, value: 1000 });
    act(() => {
      strip.dispatchEvent(new Event("scroll"));
    });
    const scrolled = spyOn(strip, "scrollBy").mockImplementation(() => {});
    spyOn(chip, "scrollIntoView").mockImplementation(() => {});
    act(() => chip.focus());
    await user.click(screen.getByRole("button", { name: "Scroll filters right" }));
    expect(scrolled).toHaveBeenCalled();
    expect(chip).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// dispatch://INFRA-2160, R12: the scroll buttons are buttons the row shows, so Tab reaches them.
test("Tab reaches a scroll button, and Enter on it that brings the strip to its end hands the focus to the chip there", async () => {
  const user = userEvent.setup();
  const view = render(
    <ControlRow
      chips={
        <>
          <button type="button">Alpha</button>
          <button type="button">Beta</button>
        </>
      }
    />
  );
  try {
    // happy-dom lays nothing out, so the strip is told it holds more than it shows, and a scroll
    // of it reads that again: the right button is drawn. Its press scrolls to the strip's end,
    // where it holds nothing more to show, and the button goes.
    const beta = screen.getByRole("button", { name: "Beta" });
    const strip = beta.parentElement;
    if (strip === null) throw new Error("the chip is drawn outside the strip");
    const overflow = (width: number) =>
      act(() => {
        Object.defineProperty(strip, "scrollWidth", { configurable: true, value: width });
        strip.dispatchEvent(new Event("scroll"));
      });
    overflow(1000);
    spyOn(strip, "scrollBy").mockImplementation(() => overflow(0));
    spyOn(beta, "scrollIntoView").mockImplementation(() => {});
    act(() => beta.focus());
    await user.tab();
    const right = screen.getByRole("button", { name: "Scroll filters right" });
    expect(right).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(screen.queryByRole("button", { name: "Scroll filters right" })).toBeNull();
    expect(beta).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/**
 * The chips strip `chip` is drawn in, which happy-dom lays out not at all (its `clientWidth` is
 * 0): the test sets how far it is scrolled from its start (`scrollLeft`) and how wide its content
 * is (`scrollWidth`), and `readEnds` fires the scroll event on which the row reads the strip's
 * ends again - the event a browser fires a frame after a focus scrolled the strip, not as it
 * scrolls.
 */
function laidOutStrip(chip: HTMLElement): {
  readEnds: () => void;
  set: (to: { scrollLeft?: number; scrollWidth?: number }) => void;
} {
  const strip = chip.parentElement;
  if (strip === null) throw new Error("the chip is drawn outside the strip");
  let scrollLeft = 0;
  let scrollWidth = 0;
  Object.defineProperty(strip, "scrollLeft", { configurable: true, get: () => scrollLeft });
  Object.defineProperty(strip, "scrollWidth", { configurable: true, get: () => scrollWidth });
  return {
    readEnds: () =>
      act(() => {
        strip.dispatchEvent(new Event("scroll"));
      }),
    set: (to) => {
      scrollLeft = to.scrollLeft ?? scrollLeft;
      scrollWidth = to.scrollWidth ?? scrollWidth;
    },
  };
}

// dispatch://INFRA-2160, R12. Tab onto the last chip scrolls the strip to its end, but the row
// reads its ends on the scroll event that comes a frame later: a Tab in between lands on the right
// button, still drawn, which then goes while it holds the focus. The reader was going on past the
// strip, so the focus goes on, not back to the chip they had just left (CI run 44557669985: "the
// keyboard reached 10 of 9 chips").
test("Tab onto the right scroll button as the strip reaches its end hands the focus on to Clear filters, not back to the last chip", async () => {
  const user = userEvent.setup();
  const view = render(<RemovableChips names={["Alpha", "Beta"]} />);
  try {
    const beta = screen.getByRole("button", { name: "Beta" });
    const strip = laidOutStrip(beta);
    strip.set({ scrollWidth: 1000 });
    strip.readEnds();
    // Beta's focus scrolls it into view, which brings the strip to its end; the event that tells
    // the row so is still to come.
    spyOn(beta, "scrollIntoView").mockImplementation(() => strip.set({ scrollLeft: 1000 }));
    act(() => beta.focus());
    await user.tab();
    expect(screen.getByRole("button", { name: "Scroll filters right" })).toHaveFocus();
    strip.readEnds();
    expect(screen.queryByRole("button", { name: "Scroll filters right" })).toBeNull();
    expect(screen.getByRole("button", { name: "Clear filters" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

// The same, the other way: Shift+Tab onto the first chip scrolls the strip to its start, and a
// Shift+Tab before the row reads that lands on the left button, which then goes.
test("Shift+Tab onto the left scroll button as the strip reaches its start hands the focus back to the control before the strip, not on to the first chip", async () => {
  const user = userEvent.setup();
  const view = render(<RemovableChips names={["Alpha", "Beta"]} />);
  try {
    const alpha = screen.getByRole("button", { name: "Alpha" });
    const strip = laidOutStrip(alpha);
    strip.set({ scrollLeft: 500, scrollWidth: 500 });
    strip.readEnds();
    // Alpha's focus brings the strip back to its start, and the event is still to come.
    spyOn(alpha, "scrollIntoView").mockImplementation(() => strip.set({ scrollLeft: 0 }));
    act(() => alpha.focus());
    await user.tab({ shift: true });
    expect(screen.getByRole("button", { name: "Scroll filters left" })).toHaveFocus();
    strip.readEnds();
    expect(screen.queryByRole("button", { name: "Scroll filters left" })).toBeNull();
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();
  } finally {
    view.unmount();
  }
});

/** The `scope` pushed, as the page that draws a row pushes its own. */
function Scoped({ children, scope }: { children: ReactNode; scope: KeymapScope }) {
  useKeymapScope(scope);
  return children;
}

/**
 * A page's row with a filter panel and a display menu, its keys under `keyScope`, in the Inbox's
 * scope. `storedOpen` opens the filter panel as the row is drawn, moving no focus into it, as the
 * Board's stored choice does.
 */
function KeyedRow({
  keyScope,
  storedOpen = false,
}: {
  keyScope?: ControlRowProps["keyScope"];
  storedOpen?: boolean;
}) {
  const [filterOpen, setFilterOpen] = useState(storedOpen);
  const [pressedOpen, setPressedOpen] = useState(false);
  const [viewOpen, setViewOpen] = useState(false);
  const [sort, setSort] = useState("newest");
  return (
    <ControlRow
      display={
        <DisplayMenu
          label="View"
          onOpenChange={setViewOpen}
          open={viewOpen}
          sections={[
            {
              id: "sort",
              label: "Sort by",
              onChange: setSort,
              options: [
                { label: "Newest first", value: "newest" },
                { label: "Priority", value: "priority" },
              ],
              value: sort,
            },
          ]}
        />
      }
      keyScope={keyScope}
      search={<input aria-label="Search" type="search" />}
      trigger={
        <PanelTrigger
          focusOnOpen={pressedOpen}
          label="Filter"
          onOpenChange={(open) => {
            setPressedOpen(open);
            setFilterOpen(open);
          }}
          open={filterOpen}
          stacked
        >
          {() => (
            <>
              <button type="button">Project</button>
              <button type="button">Priority</button>
            </>
          )}
        </PanelTrigger>
      }
    >
      <button type="button">Mark all read</button>
    </ControlRow>
  );
}

/** Takes the focus off whatever holds it, onto the page itself. */
function focusPage(): void {
  act(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
}

// dispatch://INFRA-2160, R24.
test("in the page's scope Shift+F and Shift+V open the filter panel and the display menu on their first control, and Escape hands the focus back to where it was", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Scoped scope="inbox">
        <KeyedRow keyScope="inbox" />
      </Scoped>
    </KeymapProvider>
  );
  try {
    // From the page itself, with nothing focused: Escape leaves the reader there.
    focusPage();
    await user.keyboard("{Shift>}F{/Shift}");
    expect(screen.getByRole("dialog", { name: "Filter" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Project" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Filter" })).toBeNull();
    expect(document.activeElement).toBe(document.body);

    // From a control of the row: Escape goes back to it, and Tab out goes to the trigger.
    const markAll = screen.getByRole("button", { name: "Mark all read" });
    act(() => markAll.focus());
    await user.keyboard("{Shift>}V{/Shift}");
    expect(screen.getByRole("dialog", { name: "View" })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: "Newest first" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "View" })).toBeNull();
    expect(markAll).toHaveFocus();
    await user.keyboard("{Shift>}F{/Shift}");
    await user.tab();
    await user.tab();
    expect(screen.queryByRole("dialog", { name: "Filter" })).toBeNull();
    expect(screen.getByRole("button", { name: "Filter" })).toHaveFocus();

    // Typed into the search box, Shift+F is a capital F.
    const search = screen.getByRole("searchbox", { name: "Search" });
    await user.click(search);
    await user.keyboard("{Shift>}F{/Shift}");
    expect(search).toHaveValue("F");
    expect(screen.queryByRole("dialog", { name: "Filter" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("Shift+F on a filter panel that already stands open puts the focus in it and closes nothing", async () => {
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Scoped scope="board">
        <KeyedRow keyScope="board" storedOpen />
      </Scoped>
    </KeymapProvider>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Filter" });
    const panel = screen.getByRole("dialog", { name: "Filter" });
    expect(trigger).toHaveAttribute("aria-controls", panel.id);
    focusPage();
    await user.keyboard("{Shift>}F{/Shift}");
    expect(screen.getByRole("dialog", { name: "Filter" })).toBe(panel);
    expect(screen.getByRole("button", { name: "Project" })).toHaveFocus();
    await user.keyboard("{Shift>}F{/Shift}");
    expect(screen.getByRole("dialog", { name: "Filter" })).toBe(panel);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog", { name: "Filter" })).toBeNull();
    expect(document.activeElement).toBe(document.body);
    // The Board has no display menu, so its Shift+V opens nothing.
    await user.keyboard("{Shift>}V{/Shift}");
    expect(screen.queryByRole("dialog", { name: "View" })).toBeNull();
  } finally {
    view.unmount();
  }
});

test("each scope's row registers its own scope's actions, the drawer's the Inbox's, and a row with no scope registers none", () => {
  const registered = (keyScope?: ControlRowProps["keyScope"]) => {
    const view = render(
      <KeymapProvider>
        <KeyedRow keyScope={keyScope} />
      </KeymapProvider>
    );
    try {
      return appKeymap
        .describe()
        .filter((entry) => entry.id === "filter" || entry.id === "display")
        .map((entry) => `${entry.scope} ${entry.id} ${entry.keys.join(",")}`);
    } finally {
      view.unmount();
    }
  };
  expect(registered("inbox")).toEqual(["inbox filter Shift+F", "inbox display Shift+V"]);
  expect(registered("dialog")).toEqual(["dialog filter Shift+F", "dialog display Shift+V"]);
  expect(registered("project")).toEqual(["project filter Shift+F", "project display Shift+V"]);
  expect(registered("board")).toEqual(["board filter Shift+F"]);
  expect(registered("agents")).toEqual(["agents filter Shift+F"]);
  expect(registered("delivery")).toEqual(["delivery filter Shift+F"]);
  expect(registered(undefined)).toEqual([]);
});
