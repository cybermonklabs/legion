import { expect, test } from "bun:test";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, type ReactNode } from "react";

import { SelectionBar } from "./SelectionBar";

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
 *  the page a hand-off is read on. The bar is drawn while `count` is above zero. */
function MarkedList({
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
        <input aria-label={`Select ${key}`} key={key} type="checkbox" />
      ))}
      <SelectionBar count={count} label="Selected rows" returnFocus={returnFocus}>
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
