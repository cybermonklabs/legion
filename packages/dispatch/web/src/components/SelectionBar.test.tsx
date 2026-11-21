import { expect, test } from "bun:test";
import { act, render, renderHook, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, type ReactNode, useRef, useState } from "react";

import { drawOver, layRowsOut, watchMoves } from "../__tests__/laid-out-rows";
import { useFocusRescue } from "./focus-place";
import { ROW_MARKER } from "./row-marker";
import { SelectionBar, useLastMarkedPlace } from "./SelectionBar";

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

test("the bar going while a control inside it holds the focus hands the focus to what returnFocus names", () => {
  // Whether the bar was still in the page when it was asked where the focus goes.
  const asked: string[] = [];
  const returnFocus = () => {
    asked.push(screen.queryByRole("toolbar") === null ? "after the bar went" : "with the bar up");
    return rowBox("CORE-2");
  };
  const view = render(<MarkedList count={2} returnFocus={returnFocus} />);
  try {
    act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
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
    // On the body, where the reader had left it before the bar went: the bar never held it, so
    // it is not the bar's to hand on.
    view.rerender(<MarkedList count={1} returnFocus={returnFocus} />);
    act(() => filter.blur());
    view.rerender(<MarkedList count={0} returnFocus={returnFocus} />);
    expect(document.activeElement).toBe(document.body);
    expect(asked).toBe(0);
    // The same going with the focus inside the bar does hand it on: what kept it still above is
    // that the bar did not hold it.
    view.rerender(<MarkedList count={1} returnFocus={returnFocus} />);
    act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
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
    const clear = screen.getByRole("button", { name: "Clear selection" });
    act(() => clear.focus());
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
    // The same going with the focus still on Select all does hand it on: what kept it still
    // above is that the reader had moved the focus off.
    view.rerender(<MarkedList count={1} returnFocus={returnFocus} selectAll={selectAll} />);
    act(() => selectAllButton().focus());
    view.rerender(<MarkedList count={3} returnFocus={returnFocus} />);
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toBe(1);
  } finally {
    view.unmount();
  }
});

// INFRA-3135's review round 1, R10: the bar's hand-off keeps the rescue's rule. The bar's line
// of actions scrolls sideways, and the reader had scrolled it so far that the focused Clear
// selection was out of their sight when a refetch emptied the selection and took the bar away.
// Read from the rescue's own record of what the reader can see, the bar hands the focus to no
// row, so the list is not scrolled to one the reader did not pick.
test("the bar going with a control the reader scrolled out of sight hands the focus to nothing", () => {
  const rescue = renderHook(() => useFocusRescue({ current: null }));
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedList count={2} returnFocus={returnFocus} />);
  try {
    const clear = screen.getByRole("button", { name: "Clear selection" });
    act(() => clear.focus());
    // The line 300 px wide at the foot of the window, scrolled until Clear selection is 400 px
    // past its left edge.
    const line = clear.parentElement as HTMLElement;
    line.getBoundingClientRect = () => new DOMRect(0, 700, 300, 44);
    clear.getBoundingClientRect = () => new DOMRect(-400, 704, 120, 36);
    act(() => {
      line.dispatchEvent(new Event("scroll"));
    });
    view.rerender(<MarkedList count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
    expect(asked).toBe(0);
  } finally {
    view.unmount();
    rescue.unmount();
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
// did not pick. The rescue, mounted here, is what records whether the reader pressed.
test("the bar emptied by a refetch with no press hands the focus to no row", () => {
  const rescue = renderHook(() => useFocusRescue({ current: null }));
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedTable count={2} returnFocus={returnFocus} />);
  try {
    act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
    view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).not.toHaveFocus();
    expect(document.activeElement).toBe(document.body);
    expect(asked).toBe(0);
  } finally {
    view.unmount();
    rescue.unmount();
  }
});

// Emptied by the reader's own Clear selection, a press on the bar: the bar hands the focus on as it
// always has, to what the page names.
test("the bar emptied by the reader's Clear selection hands the focus to what returnFocus names", () => {
  const rescue = renderHook(() => useFocusRescue({ current: null }));
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedTable count={2} returnFocus={returnFocus} />);
  try {
    const clear = screen.getByRole("button", { name: "Clear selection" });
    act(() => clear.focus());
    act(() => clear.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true })));
    view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toBe(1);
  } finally {
    view.unmount();
    rescue.unmount();
  }
});

// INFRA-3135, round 18 (the tester's): emptied by the reader's press on the last marked row's
// mark, the bar hands the focus on too (`pressOnRowMark`). A click leaves a checkbox unfocused in
// WebKit, so the focus is still in the bar when the press takes the last row away, and the press
// is on no control of the bar. With `pressOnRowMark` answering false in a scratch copy of
// e9ed2743, this file, `DataTable.test.tsx`, `Inbox.test.tsx` and `IssuesTable.test.tsx` passed
// 254 of 254, and so did the page-own press row of `e2e/table-controls.e2e.ts` in WebKit and
// Firefox (builds Job infra-3135-r18mk): no test read the rule.
test("the bar emptied by the reader's press on the last row's mark hands the focus to what returnFocus names", () => {
  const rescue = renderHook(() => useFocusRescue({ current: null }));
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const view = render(<MarkedTable count={1} returnFocus={returnFocus} />);
  try {
    act(() => screen.getByRole("button", { name: "Clear selection" }).focus());
    const mark = rowBox("CORE-1");
    act(() => mark.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true })));
    view.rerender(<MarkedTable count={0} returnFocus={returnFocus} />);
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
    expect(rowBox("CORE-2")).toHaveFocus();
    expect(asked).toBe(1);
  } finally {
    view.unmount();
    rescue.unmount();
  }
});

/** The keys of `PlacedList`'s twenty rows, in its order. */
const PLACED_KEYS = Array.from({ length: 20 }, (_, index) => `CORE-${index + 1}`);

/** Rows, each an `<li>` with its mark, in a list, and the bar over them with one action: the
 *  page the hand-off where the row marked last cannot take the focus is read on
 *  (`useLastMarkedPlace`). `listed` names the rows the list draws, `leaving` the drawn ones a
 *  press is taking away, and `last` the row marked last, null where no row is marked. Its
 *  `returnFocus` names the mark of the row marked last while the list holds it, as the Inbox's
 *  does; the hand-off moves the focus itself otherwise. */
function PlacedList({
  count,
  last,
  leaving = [],
  listed,
}: {
  count: number;
  last: string | null;
  leaving?: readonly string[];
  listed: readonly string[];
}): ReactNode {
  const list = useRef<HTMLUListElement>(null);
  const markOf = (key: string) =>
    list.current?.querySelector<HTMLElement>(`[data-row="${key}"] input`) ?? null;
  const landAtPlace = useLastMarkedPlace({
    control: markOf,
    lastMarked: () => last,
    leaving: (key) => leaving.includes(key),
    list: () => list.current,
    order: () =>
      [...(list.current?.querySelectorAll<HTMLElement>("[data-row]") ?? [])].map(
        (row) => row.dataset.row ?? ""
      ),
    shown: count > 0,
  });
  return (
    <>
      <ul aria-label="Rows" ref={list}>
        {listed.map((key) => (
          <li data-row={key} key={key}>
            <input aria-label={`Select ${key}`} type="checkbox" />
          </li>
        ))}
      </ul>
      <SelectionBar
        count={count}
        label="Selected rows"
        returnFocus={() => (landAtPlace() || last === null ? null : markOf(last))}
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

// A press deep in a long list takes away the two rows marked, the row marked last among them.
// The row after them moves up into its place, where the reader can see it, and takes the focus
// with no scroll. The list's first row, nine rows above the window, would scroll the list back
// to its top and throw the reader's place away.
test("a press that takes away the row marked last hands the focus to the row that moves up into its place, with no scroll", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={2} last="CORE-13" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
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

// The press took away the row marked last at the window's top, and the row that moves up into its
// place stands under the list's sticky head: its box is in sight, but the page draws the head at
// its mark's centre. No row before that place is in sight, so the list takes the focus at its own
// stop, with no scroll, rather than a row the head covers.
test("a press that takes away the row marked last, with the row now at its place under the list's sticky head, hands the focus to the list's own stop", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-10" listed={PLACED_KEYS} />);
  // The head is 60 px tall, over the top of the row at 0 to 100 px.
  const head = document.createElement("div");
  const drawn = drawOver(head, 60, "[data-row]", 9);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
    view.rerender(<PlacedList count={0} last="CORE-10" listed={without(PLACED_KEYS, "CORE-10")} />);
    const list = screen.getByRole("list", { name: "Rows" });
    expect(list).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: list });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    drawn();
    view.unmount();
    laidOut();
  }
});

// The row marked last was below the window when the press took it away, so the row that moves
// up into its place is below it too. The focus goes to the nearest row before that place that
// the reader can see, the window's last row, with no scroll.
test("a press that takes away the row marked last, with the row now at its place out of sight, hands the focus to the nearest row before it in sight", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-19" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
    view.rerender(<PlacedList count={0} last="CORE-19" listed={without(PLACED_KEYS, "CORE-19")} />);
    // CORE-20 is drawn 900 px down, under the window's 768 px; CORE-18 at 800 px is under it too.
    const landing = rowBox("CORE-17");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// A row the page is still drawing while the press takes it away is passed over: the focus would
// drop with it. The row after it is drawn under the window until then, and it moves up into the
// leaving row's place once that row goes, so the place is read from the leaving row, which the
// reader can see: the focus goes to the row after it, with no scroll.
test("a press still taking away the row marked last hands the focus to the row after it, which moves up into its place in sight", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-17" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
    view.rerender(
      <PlacedList count={0} last="CORE-17" leaving={["CORE-17"]} listed={PLACED_KEYS} />
    );
    const landing = rowBox("CORE-18");
    expect(landing).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: landing });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// The reader marked a row near the list's top, then scrolled far down it. The press takes that
// row away, and neither the row now at its place nor any row before it is in sight. The list
// itself takes the focus at its own stop, with no scroll, rather than a row above the window.
test("a press that takes away the row marked last, with no row in sight at its place or before it, hands the focus to the list's own stop", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-3" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
    view.rerender(<PlacedList count={0} last="CORE-3" listed={without(PLACED_KEYS, "CORE-3")} />);
    const list = screen.getByRole("list", { name: "Rows" });
    expect(list).toHaveFocus();
    expect(list).toHaveAttribute("tabindex", "-1");
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: list });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
});

// A press that leaves the row marked last in the list, out of sight above the window, is no
// case of the place's: the row's mark takes the focus as the page names it, and the bar brings
// it into view, as it always has.
test("the row marked last still listed takes the focus as the page names it, out of sight or not", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={1} last="CORE-3" listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
    view.rerender(<PlacedList count={0} last="CORE-3" listed={PLACED_KEYS} />);
    const mark = rowBox("CORE-3");
    expect(mark).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: undefined, target: mark });
  } finally {
    moves.restore();
    view.unmount();
    laidOut();
  }
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
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
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
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
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

// With no row marked, a row a press is still taking away is passed over as well: the focus would
// drop with it. The window's top row is that row, so the focus goes to the row below it.
test("with no row marked, the first row in sight that a press is still taking away is passed over", () => {
  const laidOut = layRowsOut("[data-row]", 9);
  const view = render(<PlacedList count={20} last={null} listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
    view.rerender(<PlacedList count={0} last={null} leaving={["CORE-10"]} listed={PLACED_KEYS} />);
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

// With no row marked and no row in sight - the press emptied the list - the list itself takes
// the focus at its own stop, with no scroll, and the focus does not drop onto the page's body.
test("with no row marked and no row left, the bar gone with the focus hands it to the list's own stop", () => {
  const view = render(<PlacedList count={20} last={null} listed={PLACED_KEYS} />);
  const moves = watchMoves();
  try {
    act(() => screen.getByRole("button", { name: "Dismiss" }).focus());
    view.rerender(<PlacedList count={0} last={null} listed={[]} />);
    const list = screen.getByRole("list", { name: "Rows" });
    expect(list).toHaveFocus();
    expect(moves.focused.at(-1)).toEqual({ options: { preventScroll: true }, target: list });
    expect(moves.scrolled).toEqual([]);
  } finally {
    moves.restore();
    view.unmount();
  }
});
