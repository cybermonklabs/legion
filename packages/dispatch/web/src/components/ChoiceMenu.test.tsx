import { expect, spyOn, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the bare mouse, pointer and touch presses a draggable parent must not see, which user-event has no form for
import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { pickNamed } from "../__tests__/list-picks";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { useKeymap, useKeymapScope } from "../features/shell/keymap";
import { ChoiceMenu, type MenuChoice } from "./ChoiceMenu";
import { armOnPress, causedByReader } from "./focus-place";
import { useHeldThroughAnswer } from "./held-press";

const CHOICES: readonly MenuChoice<string>[] = [
  { label: "Move left", value: "left" },
  { label: "Move right", value: "right" },
];

/** A page with keys of its own on the keys a menu takes - the arrows and `j` - a row a key
 *  presses the trigger from, a control and some prose beside it, and the menu. Every key the page
 *  takes, and every value the menu writes, is noted. */
function Page({ chosen, taken }: { chosen: string[]; taken: string[] }) {
  useKeymapScope("inbox");
  useKeymap("inbox", [
    { action: "inbox.next", run: (event) => taken.push(event.key) },
    { action: "inbox.arrows", run: (event) => taken.push(event.key) },
  ]);
  return (
    <>
      <button type="button">Row</button>
      <ChoiceMenu
        choices={CHOICES}
        face="Move"
        label="Move Status"
        marks={{ "data-column-move": "status" }}
        onChoose={(value) => chosen.push(value)}
        triggerClassName=""
      >
        <span>Move</span>
      </ChoiceMenu>
      <button type="button">Elsewhere</button>
      <p>Some prose beside the menu.</p>
    </>
  );
}

function renderPage() {
  const chosen: string[] = [];
  const taken: string[] = [];
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Page chosen={chosen} taken={taken} />
    </KeymapProvider>
  );
  const trigger = screen.getByRole("button", { name: pickNamed("Move Status") });
  const row = screen.getByRole("button", { name: "Row" });
  return { chosen, row, taken, trigger, unmount: view.unmount, user };
}

const menu = () => screen.queryByRole("menu", { name: "Move Status" });
const item = (name: string) => screen.getByRole("menuitem", { name });

test("a press on the trigger opens a menu of its choices on the first, with the menu's keys below", async () => {
  const { chosen, trigger, unmount, user } = renderPage();
  try {
    expect(trigger).toHaveAttribute("aria-haspopup", "menu");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(trigger).not.toHaveAttribute("aria-controls");
    await user.click(trigger);

    const opened = menu();
    expect(opened).toBeInTheDocument();
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    expect(trigger).toHaveAttribute("aria-controls", opened?.id ?? "");
    const items = within(opened as HTMLElement).getAllByRole("menuitem");
    expect(items.map((choice) => choice.textContent)).toEqual(["Move left", "Move right"]);
    expect(item("Move left")).toHaveFocus();
    expect(items.map((choice) => choice.tabIndex)).toEqual([0, -1]);
    expect(opened).toHaveTextContent("↑ ↓ Home End to move · Enter to choose · Esc to close");
    expect(chosen).toEqual([]);
  } finally {
    unmount();
  }
});

test("ArrowDown, ArrowUp, Home and End move the highlight, write nothing and reach no page key", async () => {
  const { chosen, taken, trigger, unmount, user } = renderPage();
  try {
    await user.click(trigger);
    await user.keyboard("{ArrowDown}");
    expect(item("Move right")).toHaveFocus();
    expect(item("Move right").tabIndex).toBe(0);
    expect(item("Move left").tabIndex).toBe(-1);
    await user.keyboard("{ArrowUp}");
    expect(item("Move left")).toHaveFocus();
    // The arrows wrap at the ends.
    await user.keyboard("{ArrowUp}");
    expect(item("Move right")).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(item("Move left")).toHaveFocus();
    await user.keyboard("{End}");
    expect(item("Move right")).toHaveFocus();
    await user.keyboard("{Home}");
    expect(item("Move left")).toHaveFocus();
    await user.keyboard("j");
    expect(menu()).toBeInTheDocument();
    expect(chosen).toEqual([]);
    expect(taken).toEqual([]);
  } finally {
    unmount();
  }
});

test("Enter writes the highlighted choice once, closes the menu and hands the focus back", async () => {
  const { chosen, trigger, unmount, user } = renderPage();
  try {
    await user.click(trigger);
    await user.keyboard("{ArrowDown}{Enter}");
    expect(chosen).toEqual(["right"]);
    expect(menu()).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
  } finally {
    unmount();
  }
});

test("a click writes the choice it hits once", async () => {
  const { chosen, trigger, unmount, user } = renderPage();
  try {
    await user.click(trigger);
    await user.click(item("Move right"));
    expect(chosen).toEqual(["right"]);
    expect(menu()).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    unmount();
  }
});

test("Escape, Tab, Shift+Tab and a press outside close the menu and write nothing", async () => {
  const { chosen, trigger, unmount, user } = renderPage();
  try {
    await user.click(trigger);
    await user.keyboard("{ArrowDown}{Escape}");
    expect(menu()).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();

    await user.click(trigger);
    await user.keyboard("{ArrowDown}");
    await user.tab();
    expect(menu()).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();

    await user.click(trigger);
    await user.tab({ shift: true });
    expect(menu()).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();

    await user.click(trigger);
    await user.click(screen.getByText("Some prose beside the menu."));
    expect(menu()).not.toBeInTheDocument();

    // A second press on the trigger closes it too.
    await user.click(trigger);
    await user.click(trigger);
    expect(menu()).not.toBeInTheDocument();
    expect(chosen).toEqual([]);
  } finally {
    unmount();
  }
});

test("the focus goes back to the row a key pressed the trigger from, and to the trigger after a press on it", async () => {
  const { chosen, row, trigger, unmount, user } = renderPage();
  try {
    // A key's binding presses the trigger from the row, as a page's key presses a control from
    // the row it acts on: a click with no pointer, while the focus is still on the row.
    act(() => row.focus());
    act(() => trigger.click());
    expect(item("Move left")).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(row).toHaveFocus();

    act(() => trigger.click());
    await user.keyboard("{ArrowDown}");
    await user.tab();
    expect(row).toHaveFocus();

    act(() => trigger.click());
    await user.keyboard("{End}{Enter}");
    expect(chosen).toEqual(["right"]);
    expect(row).toHaveFocus();

    // A press outside on nothing that takes the focus hands it back to the row once the press is
    // over; a press on a control leaves it on that control, which the press acts on.
    act(() => trigger.click());
    await user.click(screen.getByText("Some prose beside the menu."));
    expect(menu()).not.toBeInTheDocument();
    expect(row).toHaveFocus();
    act(() => trigger.click());
    const elsewhere = screen.getByRole("button", { name: "Elsewhere" });
    await user.click(elsewhere);
    expect(menu()).not.toBeInTheDocument();
    expect(elsewhere).toHaveFocus();

    // Enter on the trigger is a press with no pointer too, from the trigger itself.
    act(() => trigger.focus());
    await user.keyboard("{Enter}");
    expect(item("Move left")).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();

    // A pointer's press is on the trigger, whatever held the focus before it.
    act(() => row.focus());
    await user.click(trigger);
    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();
    expect(chosen).toEqual(["right"]);
  } finally {
    unmount();
  }
});

// The rule of the shared single pick's press outside (`MultiSelect`'s `handBackOverPress`, the
// tester's round 16 of dispatch://INFRA-2160): the focus a press outside gives back, at its
// `pointerdown` and again once it is over, comes with no scroll. A scroll then would move the
// page under the press between its `mousedown` and its `mouseup`, so a press on a control far
// from the trigger would release on another element and not act. Escape and Tab give the focus
// back as the engine focuses, which brings the trigger into view where it is out of sight.
test("a press outside gives the focus back with no scroll, and Escape and Tab as the engine focuses", async () => {
  const { chosen, trigger, unmount, user } = renderPage();
  const focus = spyOn(trigger, "focus");
  // How each focus given to the trigger since the menu opened asked to scroll.
  const givenBack = async (close: () => Promise<void>) => {
    await user.click(trigger);
    expect(menu()).toBeInTheDocument();
    const from = focus.mock.calls.length;
    await close();
    expect(menu()).not.toBeInTheDocument();
    return focus.mock.calls.slice(from).map(([options]) => options);
  };
  try {
    const noScroll = { preventScroll: true };
    expect(
      await givenBack(() => user.click(screen.getByRole("button", { name: "Elsewhere" })))
    ).toEqual([noScroll]);
    expect(
      await givenBack(() => user.click(screen.getByText("Some prose beside the menu.")))
    ).toEqual([noScroll, noScroll]);
    expect(trigger).toHaveFocus();
    for (const keys of ["{Escape}", "{Tab}"]) {
      expect(await givenBack(() => user.keyboard(keys)), keys).toEqual([undefined]);
      expect(trigger, keys).toHaveFocus();
    }
    expect(chosen).toEqual([]);
  } finally {
    focus.mockRestore();
    unmount();
  }
});

test("a press on the trigger or in its menu stays out of a draggable parent", async () => {
  const reached: string[] = [];
  const user = userEvent.setup();
  const view = render(
    <article
      aria-label="Status column"
      onMouseDown={() => reached.push("mousedown")}
      onPointerDown={() => reached.push("pointerdown")}
      onTouchStart={() => reached.push("touchstart")}
    >
      <ChoiceMenu
        choices={CHOICES}
        face="Move"
        label="Move Status"
        marks={{}}
        onChoose={() => undefined}
        triggerClassName=""
      >
        Move
      </ChoiceMenu>
    </article>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Move Status" });
    // Bare mouse, pointer and touch presses: user-event sends a whole press sequence and cannot
    // deliver one of these events on its own.
    fireEvent.mouseDown(trigger);
    fireEvent.pointerDown(trigger);
    fireEvent.touchStart(trigger);
    await user.keyboard("{Tab}{Enter}");
    // The menu is portalled to the body, but it is a React child of the header cell.
    fireEvent.mouseDown(item("Move right"));
    fireEvent.pointerDown(item("Move right"));
    fireEvent.touchStart(item("Move right"));
    expect(menu()).toBeInTheDocument();
    expect(reached).toEqual([]);
  } finally {
    view.unmount();
  }
});

/** Two keyed rows of one list, the first with a menu whose pick moves its row below the other,
 *  as a column's move draws its header cell at the column's new place: React takes the row's node
 *  out and puts it back. Each row arms the page's hand-off at its own presses (`armOnPress`), as a
 *  component that hands its focus off arms it at its root. With `holds`, the pick's write is out
 *  from then on, and holds the press's hand-off while it is (`useHeldThroughAnswer`). */
function MovingRows({ holds = false }: { holds?: boolean }) {
  const [order, setOrder] = useState(["column-a", "column-b"]);
  const [writing, setWriting] = useState(false);
  useHeldThroughAnswer(writing);
  return (
    <ul>
      {order.map((id) => (
        <li
          data-row={id}
          key={id}
          onChangeCapture={armOnPress}
          onClickCapture={armOnPress}
          onKeyDownCapture={armOnPress}
        >
          {id === "column-a" ? (
            <ChoiceMenu
              choices={CHOICES}
              face="Move"
              label="Move Status"
              marks={{}}
              onChoose={() => {
                setOrder(["column-b", "column-a"]);
                if (holds) setWriting(true);
              }}
              triggerClassName=""
            >
              Move
            </ChoiceMenu>
          ) : (
            <button type="button">Other row</button>
          )}
        </li>
      ))}
    </ul>
  );
}

// INFRA-2160: a pick in a menu is the reader's press in the row its trigger stands in, though the
// menu is portalled out of that row: React hands a press in the menu to the row's own handlers,
// which arm the hand-off at the row (`armOnPress`). So the focus the pick's write takes away with
// the row is a loss the reader caused (`causedByReader`), rather than one to leave on the body.
// The press is that cause while the write it made is out, which holds it armed
// (`useHeldThroughAnswer`, `holdHandOff`).
test("a pick in the menu is the reader's own press on its trigger and the row it sits in", async () => {
  const user = userEvent.setup();
  const view = render(<MovingRows holds />);
  try {
    const trigger = screen.getByRole("button", { name: "Move Status" });
    const other = screen.getByRole("button", { name: "Other row" });
    act(() => trigger.focus());
    await user.keyboard("{Enter}");
    await user.keyboard("{ArrowDown}{Enter}");
    expect(causedByReader(trigger)).toBe(true);
    // A row the press was not in, nor the menu it landed in, is not the reader's doing.
    expect(causedByReader(other)).toBe(false);
  } finally {
    view.unmount();
  }
});

// INFRA-2160, R2 at 390 px: the pick hands the focus back to the trigger, and its write then moves
// the row down the page - a column's move draws its header cell at the column's new place - out
// of the window. The trigger is kept in sight as it moves, so its later going is not read as one
// the reader scrolled away from, until the reader's own next act.
test("the element a pick hands the focus back to is kept in view as the write moves it, until the reader acts", async () => {
  const user = userEvent.setup();
  const view = render(<MovingRows />);
  try {
    const trigger = screen.getByRole("button", { name: "Move Status" });
    const scrolled: unknown[] = [];
    trigger.scrollIntoView = (options) => scrolled.push(options);
    act(() => trigger.focus());
    await user.keyboard("{Enter}");
    // Drawn in the window while its row is first, below the window's foot once the write has
    // moved its row down.
    trigger.getBoundingClientRect = () =>
      DOMRect.fromRect({
        height: 44,
        width: 80,
        x: 0,
        y: trigger.closest("li")?.previousElementSibling === null ? 10 : 5000,
      });
    await user.keyboard("{ArrowDown}{Enter}");
    expect(
      [...document.querySelectorAll("[data-row]")].map((row) => row.getAttribute("data-row"))
    ).toEqual(["column-b", "column-a"]);
    expect(trigger).toHaveFocus();
    expect(scrolled).toEqual([{ block: "nearest" }]);

    // The reader's next press ends it: a later commit moves nothing into view.
    await user.keyboard("{Shift}");
    view.rerender(<MovingRows />);
    expect(scrolled).toHaveLength(1);
  } finally {
    view.unmount();
  }
});
