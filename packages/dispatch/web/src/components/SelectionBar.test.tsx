import { expect, test } from "bun:test";
import { act, render, renderHook, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef, type ReactNode, useState } from "react";

import { ChoiceMenu } from "./ChoiceMenu";
import { useFocusRescue } from "./focus-place";
import { ROW_MARKER } from "./row-marker";
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
 *  empties the selection. The rows sit in no scroll box, so a loss the reader did not cause
 *  leaves the focus on the body rather than pulled into any row. */
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

// INFRA-2160: the bar's Snooze is a menu, portalled out of the bar, and a pick in it hands the focus
// back to the bar's trigger before its write takes the bar away. The pick is the reader's press in
// the bar, though it landed in the menu, so the bar hands the focus on to what the page names. The
// trigger is held while the write is out, as the Inbox holds it, which keeps the press the cause
// until the write lands (`answerPress`).
test("the bar emptied by a pick in a menu its own trigger opened hands the focus to what returnFocus names", async () => {
  const rescue = renderHook(() => useFocusRescue({ current: null }));
  const user = userEvent.setup();
  let asked = 0;
  const returnFocus = () => {
    asked += 1;
    return rowBox("CORE-2");
  };
  const picked: string[] = [];
  function Bar({ count }: { count: number }): ReactNode {
    const [held, setHeld] = useState(false);
    return (
      <>
        {["CORE-1", "CORE-2"].map((key) => (
          <div data-inbox-row={key} key={key}>
            <input aria-label={`Select ${key}`} type="checkbox" />
          </div>
        ))}
        <SelectionBar count={count} label="Selected rows" returnFocus={returnFocus}>
          <ChoiceMenu
            choices={[{ digit: "1", label: "Later today", value: "later" }]}
            face="Snooze"
            held={held}
            label="Snooze selected asks"
            marks={{}}
            onChoose={(value) => {
              picked.push(value);
              setHeld(true);
            }}
            triggerClassName=""
          >
            Snooze
          </ChoiceMenu>
        </SelectionBar>
      </>
    );
  }
  const view = render(<Bar count={2} />);
  try {
    const trigger = screen.getByRole("button", { name: "Snooze selected asks" });
    act(() => trigger.focus());
    await user.keyboard("{Enter}");
    expect(screen.getByRole("menuitem", { name: "Later today" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(picked).toEqual(["later"]);
    expect(trigger).toHaveFocus();
    // The write lands and the marked rows leave: the bar goes with its trigger.
    view.rerender(<Bar count={0} />);
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
