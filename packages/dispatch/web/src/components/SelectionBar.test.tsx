import { expect, spyOn, test } from "bun:test";
import { act, render, renderHook, screen } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { createRef, type ReactNode, useRef, useState } from "react";

import { disarm } from "../__tests__/armed-press";
import { SIZE_CSS, sizeUnder } from "../__tests__/control-size";
import { FootFloat } from "../__tests__/foot-float";
import { nextFrame } from "../__tests__/frames";
import { drawOver, layRowsOut, watchMoves } from "../__tests__/laid-out-rows";
import { DOCUMENT_SCROLL_ATTRIBUTE, footPassNow, MARK_ATTRIBUTE } from "../hooks/useFootInset";
import type { ControlSize } from "./control-names";
import { ARM_ON_PRESS, holdHandOff } from "./focus-place";
import { ROW_MARKER } from "./row-marker";
import {
  focusListStop,
  type MarkedList as MarkedListModel,
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
    // With no size the bar is drawn comfortable, the size the Inbox passes (`BAR_SIZE`).
    expect(padding()).toBe("8px 16px 8px 16px");
    view.rerender(bar("comfortable"));
    expect(padding()).toBe("8px 16px 8px 16px");
    view.rerender(bar("compact"));
    expect(padding()).toBe("4px 8px 4px 8px");
  } finally {
    view.unmount();
  }
});

/** Tailwind's definitions of the utilities the bar's line is drawn by below `sm`, and of the `sm:`
 *  ones as they apply from 640 px up, read after them as Tailwind orders them. */
const LINE_CSS =
  ".flex-nowrap{flex-wrap:nowrap}.flex-wrap{flex-wrap:wrap}.overflow-x-auto{overflow-x:auto}";
const SM_LINE_CSS = String.raw`.sm\:flex-wrap{flex-wrap:wrap}.sm\:overflow-x-visible{overflow-x:visible}`;

// INFRA-3135, round 29 (the architect's ruling of 2026-11-26 07:01Z): below `sm` the bar stands
// under the header over the list's top rows, and the compact line wrapped there stood 210 px over
// the rows it acts on at 390x664 (builds Jobs `infra-3135-infra-3135-r30j-dblp` and `-r30k-dblp`).
// So it keeps one line that scrolls sideways there, and wraps from `sm` up, where the bar stands at
// the foot and the foot inset gives the list its room.
test("the compact bar's line is one line that scrolls sideways below sm, and wraps from sm up", () => {
  const view = render(
    <SelectionBar count={2} label="Selected rows" size="compact">
      <button type="button">Clear selection</button>
    </SelectionBar>
  );
  const line = screen.getByRole("button", { name: "Clear selection" }).parentElement;
  if (line === null) throw new Error("the bar draws no line around its actions");
  const drawn = (css: string) => {
    const sheet = document.createElement("style");
    sheet.textContent = css;
    document.head.appendChild(sheet);
    try {
      const style = getComputedStyle(line);
      return { flexWrap: style.flexWrap, overflowX: style.overflowX };
    } finally {
      sheet.remove();
    }
  };
  try {
    expect(drawn(LINE_CSS)).toEqual({ flexWrap: "nowrap", overflowX: "auto" });
    expect(drawn(LINE_CSS + SM_LINE_CSS)).toEqual({ flexWrap: "wrap", overflowX: "visible" });
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
    // The same going with the focus still on Select all does hand it on: what kept it still
    // above is that the reader had moved the focus off.
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
// actions scrolls sideways, and the reader had scrolled it so far that the focused Clear selection
// was out of their sight when a refetch emptied the selection and took the bar away. The bar
// hands the focus to no row, so the list is not scrolled to one the reader did not pick.
test("the bar going with a control the reader scrolled out of sight hands the focus to nothing", () => {
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
 *  row nested in it, as an Inbox row draws an ask card. `head` is the list's sticky head, as the
 *  List names its table's. Its `returnFocus` ends at the list's own stop where the hand-off moves
 *  nothing, as the Inbox's does. */
function PlacedList({
  cards = false,
  count,
  drawRow,
  fit,
  head = null,
  headings = [],
  last,
  leaving = [],
  listed,
  undrawn = [],
}: {
  cards?: boolean;
  count: number;
  /** Draws a listed, undrawn row and lands on its mark once drawn (`MarkedList`'s `drawRow`). */
  drawRow?: (id: string, onLand?: (mark: HTMLElement) => void) => () => void;
  fit?: () => void;
  head?: Element | null;
  headings?: readonly string[];
  last: string | null;
  leaving?: readonly string[];
  listed: readonly string[];
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
                  {cards ? <article aria-label={`Card of ${key}`} /> : null}
                </li>,
              ]
        )}
      </ul>
      <SelectionBar
        count={count}
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
    // The host holds the press through its write, as the Inbox and the List hold theirs.
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
] as const)("%s ends what is owed, and the focus stays there once the rows have gone", async (what, stays) => {
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
    view.rerender(<PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-13")} />);
    expect(standing()).toBe(stays);
  } finally {
    keys();
    answered();
    disarm();
    outside.unmount();
    view.unmount();
    laidOut();
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
] as const)("%s's read of the focus, ended by %s before it runs, moves nothing", async (what, ending) => {
  const laidOut = layRowsOut("[data-row]", 9);
  const rows = render(
    <ul aria-label="Rows">
      {PLACED_KEYS.map((key) => (
        <li data-row={key} key={key}>
          <input aria-label={`Select ${key}`} type="checkbox" />
        </li>
      ))}
    </ul>
  );
  const stop = screen.getByRole("list", { name: "Rows" });
  const hand = renderHook(() =>
    useLastMarkedPlace({
      control: (key) => stop.querySelector<HTMLElement>(`[data-row="${key}"] input`),
      lastMarked: () => "CORE-13",
      leaving: (key) => key === "CORE-13",
      list: () => stop,
      order: () => PLACED_KEYS,
      shown: false,
    })
  );
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
});

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
  const page = (count: number, float: boolean, hidden?: readonly string[]) => (
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
          listed={PLACED_KEYS}
          undrawn={hidden}
        />
        <p data-testid={lineId}>The last line of the page.</p>
      </main>
      <FootFloat
        active={float}
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
    rerender: (count: number, float: boolean, hidden?: readonly string[]) =>
      view.rerender(page(count, float, hidden)),
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
] as const)("the bar going for a cause not the reader's with focus in %s owes nothing and a later commit lands nothing", (_kind, heldRow, afterRemoval) => {
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
});

// INFRA-3135, round 29 (the architect's rulings of 2026-11-26 04:0xZ): a landing is owed only
// until the reader's own next action (`READER_ACTS`), which is the invariant that keeps this
// hand-off from being the app-wide rescue this pull request removes. The reader acts while the
// press's rows are still listed, so the commit that takes those rows away lands nothing: their own
// action moved them on.
test.each([
  ["a key", () => new KeyboardEvent("keydown", { bubbles: true, key: "j" })],
  ["a press", () => new PointerEvent("pointerdown", { bubbles: true })],
  ["a wheel", () => new WheelEvent("wheel", { bubbles: true })],
] as const)("%s of the reader's own ends a landing that is owed", (_what, act_) => {
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
    expect(rowBox("CORE-13")).toHaveFocus();
    document.dispatchEvent(act_());
    // The reader's action takes the focus nowhere here, and the list's stop keeps it; the landing
    // the going owed is gone, so the rows going now land nothing.
    act(() => {
      (document.activeElement as HTMLElement).blur();
    });
    view.rerender(
      <PlacedList count={0} last="CORE-13" listed={without(PLACED_KEYS, "CORE-12", "CORE-13")} />
    );
    expect(document.body).toHaveFocus();
  } finally {
    answered();
    disarm();
    view.unmount();
    laidOut();
  }
});

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
    // The host holds the press through its write, as the Inbox and the List hold theirs.
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

test("Clear selection at the window's bottom edge scrolls just the cut, without the root's scroll padding", () => {
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
