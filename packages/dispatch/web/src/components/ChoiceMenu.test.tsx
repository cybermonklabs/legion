import { expect, test } from "bun:test";
// biome-ignore lint/style/noRestrictedImports: this file keeps fireEvent for the bare mouse, pointer and touch presses a draggable parent must not see, which user-event has no form for
import { act, fireEvent, render, renderHook, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { keyHintOf } from "../__tests__/key-hints";
import { pickNamed } from "../__tests__/list-picks";
import { KeymapProvider } from "../features/shell/KeymapProvider";
import { useKeymap, useKeymapScope } from "../features/shell/keymap";
import { ChoiceMenu, type MenuChoice } from "./ChoiceMenu";
import { causedByReader, useFocusRescue } from "./focus-place";

const CHOICES: readonly MenuChoice<string>[] = [
  { digit: "1", label: "Later today", value: "later" },
  { digit: "2", label: "Tomorrow", value: "tomorrow" },
  { digit: "3", label: "Next week", value: "week" },
];

/** A page with keys of its own on the keys a menu takes - the arrows, `j`, and a `2` that fires
 *  even in a text field - a row a key presses the trigger from, a control and some prose beside
 *  it, and the menu. Every key the page takes, and every value the menu writes, is noted. */
function Page({
  checked,
  chosen,
  held,
  taken,
}: {
  checked?: string;
  chosen: string[];
  held?: boolean;
  taken: string[];
}) {
  useKeymapScope("inbox");
  useKeymap("inbox", [
    { action: "inbox.next", run: (event) => taken.push(event.key) },
    { action: "inbox.arrows", run: (event) => taken.push(event.key) },
    { id: "two", inEditable: true, keys: "2", label: "Two", run: (event) => taken.push(event.key) },
  ]);
  return (
    <>
      <button type="button">Row</button>
      <ChoiceMenu
        checked={checked}
        choices={CHOICES}
        face={checked ?? "Snooze"}
        held={held}
        label="Snooze CORE-1"
        marks={{ "data-snooze-menu": "" }}
        onChoose={(value) => chosen.push(value)}
        triggerClassName=""
      >
        <span>{checked ?? "Snooze"}</span>
      </ChoiceMenu>
      <button type="button">Elsewhere</button>
      <p>Some prose beside the menu.</p>
    </>
  );
}

function renderPage(props: { checked?: string; held?: boolean } = {}) {
  const chosen: string[] = [];
  const taken: string[] = [];
  const user = userEvent.setup();
  const view = render(
    <KeymapProvider>
      <Page chosen={chosen} taken={taken} {...props} />
    </KeymapProvider>
  );
  const trigger = screen.getByRole("button", { name: pickNamed("Snooze CORE-1") });
  const row = screen.getByRole("button", { name: "Row" });
  return { chosen, row, taken, trigger, unmount: view.unmount, user };
}

const menu = () => screen.queryByRole("menu", { name: "Snooze CORE-1" });
const item = (name: string) => screen.getByRole("menuitem", { name });

test("a press on the trigger opens a menu of its choices on the first, each with its digit and the menu's keys below", async () => {
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
    expect(items.map((choice) => choice.textContent)).toEqual([
      "Later today1",
      "Tomorrow2",
      "Next week3",
    ]);
    // The digit is drawn for the eye and named to assistive tech as the item's shortcut.
    expect(items.map((choice) => choice.getAttribute("aria-keyshortcuts"))).toEqual([
      "1",
      "2",
      "3",
    ]);
    expect(item("Later today")).toHaveFocus();
    expect(items.map((choice) => choice.tabIndex)).toEqual([0, -1, -1]);
    expect(opened).toHaveTextContent(
      "↑ ↓ Home End to move · Enter or 1–3 to choose · Esc to close"
    );
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
    expect(item("Tomorrow")).toHaveFocus();
    expect(item("Tomorrow").tabIndex).toBe(0);
    expect(item("Later today").tabIndex).toBe(-1);
    await user.keyboard("{ArrowUp}");
    expect(item("Later today")).toHaveFocus();
    // The arrows wrap at the ends.
    await user.keyboard("{ArrowUp}");
    expect(item("Next week")).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(item("Later today")).toHaveFocus();
    await user.keyboard("{End}");
    expect(item("Next week")).toHaveFocus();
    await user.keyboard("{Home}");
    expect(item("Later today")).toHaveFocus();
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
    expect(chosen).toEqual(["tomorrow"]);
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
    await user.click(item("Next week"));
    expect(chosen).toEqual(["week"]);
    expect(menu()).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    unmount();
  }
});

test("a digit writes its choice at once, and no page key that takes the digit fires", async () => {
  const { chosen, taken, trigger, unmount, user } = renderPage();
  try {
    await user.click(trigger);
    await user.keyboard("2");
    expect(chosen).toEqual(["tomorrow"]);
    expect(taken).toEqual([]);
    expect(menu()).not.toBeInTheDocument();
    // A digit no choice carries does nothing in the menu.
    await user.click(trigger);
    await user.keyboard("7");
    expect(menu()).toBeInTheDocument();
    expect(chosen).toEqual(["tomorrow"]);
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
    // A key's binding presses the trigger from the row, as the Inbox's `b` does: a click with no
    // pointer, while the focus is still on the row.
    act(() => row.focus());
    act(() => trigger.click());
    expect(item("Later today")).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(row).toHaveFocus();

    act(() => trigger.click());
    await user.keyboard("{ArrowDown}");
    await user.tab();
    expect(row).toHaveFocus();

    act(() => trigger.click());
    await user.keyboard("3");
    expect(chosen).toEqual(["week"]);
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
    expect(item("Later today")).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();

    // A pointer's press is on the trigger, whatever held the focus before it.
    act(() => row.focus());
    await user.click(trigger);
    await user.keyboard("{Escape}");
    expect(trigger).toHaveFocus();
    expect(chosen).toEqual(["week"]);
  } finally {
    unmount();
  }
});

test("a menu of values draws radios with the current one checked, opens on it, and writes nothing when it is chosen again", async () => {
  const { chosen, trigger, unmount, user } = renderPage({ checked: "tomorrow" });
  try {
    // The trigger is named for its menu and then for what it draws: the value it holds.
    expect(trigger).toHaveAccessibleName("Snooze CORE-1 tomorrow");
    expect(trigger).not.toHaveAttribute("aria-describedby");
    expect(trigger).toHaveTextContent(/^tomorrow$/);
    await user.click(trigger);
    const radios = screen.getAllByRole("menuitemradio");
    expect(radios.map((radio) => radio.getAttribute("aria-checked"))).toEqual([
      "false",
      "true",
      "false",
    ]);
    expect(screen.queryAllByRole("menuitem")).toEqual([]);
    expect(screen.getByRole("menuitemradio", { name: "Tomorrow" })).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(menu()).not.toBeInTheDocument();
    expect(chosen).toEqual([]);

    await user.click(trigger);
    await user.keyboard("{ArrowDown}{Enter}");
    expect(chosen).toEqual(["week"]);
  } finally {
    unmount();
  }
});

test("a held trigger keeps its focus and opens nothing", async () => {
  const { trigger, unmount, user } = renderPage({ held: true });
  try {
    expect(trigger).toHaveAttribute("aria-disabled", "true");
    expect(trigger).not.toBeDisabled();
    await user.click(trigger);
    expect(menu()).not.toBeInTheDocument();
    act(() => trigger.click());
    expect(menu()).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    unmount();
  }
});

test("a press on the trigger or in its menu stays out of a draggable parent", async () => {
  const reached: string[] = [];
  const user = userEvent.setup();
  const view = render(
    <article
      aria-label="CORE-1 Review the spec"
      onMouseDown={() => reached.push("mousedown")}
      onPointerDown={() => reached.push("pointerdown")}
      onTouchStart={() => reached.push("touchstart")}
    >
      <ChoiceMenu
        choices={CHOICES}
        face="Snooze"
        label="Snooze CORE-1"
        marks={{}}
        onChoose={() => undefined}
        triggerClassName=""
      >
        Snooze
      </ChoiceMenu>
    </article>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Snooze CORE-1" });
    // Bare mouse, pointer and touch presses: user-event sends a whole press sequence and cannot
    // deliver one of these events on its own.
    fireEvent.mouseDown(trigger);
    fireEvent.pointerDown(trigger);
    fireEvent.touchStart(trigger);
    await user.keyboard("{Tab}{Enter}");
    // The menu is portalled to the body, but it is a React child of the card.
    fireEvent.mouseDown(item("Tomorrow"));
    fireEvent.pointerDown(item("Tomorrow"));
    fireEvent.touchStart(item("Tomorrow"));
    expect(menu()).toBeInTheDocument();
    expect(reached).toEqual([]);
  } finally {
    view.unmount();
  }
});

/** Two keyed rows of one list, the first with a menu whose pick moves its row below the other,
 *  as a snooze moves its row down into Later: React takes the row's node out and puts it back.
 *  With `holds`, the pick's write is out from then on, and the menu's trigger is held, as the
 *  Inbox holds a snooze's while its write is out. */
function MovingRows({ holds = false }: { holds?: boolean }) {
  const [order, setOrder] = useState(["ask-a", "ask-b"]);
  const [held, setHeld] = useState(false);
  return (
    <ul>
      {order.map((id) => (
        <li data-inbox-row={id} key={id}>
          {id === "ask-a" ? (
            <ChoiceMenu
              choices={CHOICES}
              face="Snooze"
              held={held}
              label="Snooze CORE-1"
              marks={{}}
              onChoose={() => {
                setOrder(["ask-b", "ask-a"]);
                if (holds) setHeld(true);
              }}
              triggerClassName=""
            >
              Snooze
            </ChoiceMenu>
          ) : (
            <button type="button">Other row</button>
          )}
        </li>
      ))}
    </ul>
  );
}

// INFRA-2160: a pick in a menu is the reader's press on the control that opened it, though the
// menu is portalled out of that control's row. So the focus the pick's write takes away with the
// row - a snooze that folds the card, a priority that moves the card into another lane, a bulk
// snooze that takes the selection bar away - is a loss the reader caused, which the page hands on
// to the control now at its place and scrolls into view, rather than leaving it on the body. The
// press is that cause while the write it made is out, its trigger held (`answerPress`).
test("a pick in the menu is the reader's own press on its trigger and the row it sits in", async () => {
  const rescue = renderHook(() => useFocusRescue({ current: null }));
  const user = userEvent.setup();
  const view = render(<MovingRows holds />);
  try {
    const trigger = screen.getByRole("button", { name: "Snooze CORE-1" });
    const other = screen.getByRole("button", { name: "Other row" });
    act(() => trigger.focus());
    await user.keyboard("{Enter}");
    await user.keyboard("2");
    expect(causedByReader(trigger)).toBe(true);
    // A row the press was not in, nor the menu it landed in, is not the reader's doing.
    expect(causedByReader(other)).toBe(false);
  } finally {
    view.unmount();
    rescue.unmount();
  }
});

// INFRA-2160, R2 at 390 px: the pick hands the focus back to the trigger, and its write then moves
// the row down the page - a snooze takes it down into Later - out of the window. The trigger is
// kept in sight as it moves, so its later going is not read as one the reader scrolled away from,
// until the reader's own next act.
test("the element a pick hands the focus back to is kept in view as the write moves it, until the reader acts", async () => {
  const user = userEvent.setup();
  const view = render(<MovingRows />);
  try {
    const trigger = screen.getByRole("button", { name: "Snooze CORE-1" });
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
    await user.keyboard("2");
    expect(
      [...document.querySelectorAll("[data-inbox-row]")].map((row) =>
        row.getAttribute("data-inbox-row")
      )
    ).toEqual(["ask-b", "ask-a"]);
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

// Where its host draws its keys (the Inbox's reading pane), the trigger names each of them to
// assistive tech and draws the first inside it for the eye, outside what it is called and as no
// text of it: its name is its menu's and then what it shows, and its words are what it shows.
test("a trigger given its keys names them all and draws the first, outside its name and its text", () => {
  const view = render(
    <ChoiceMenu
      checked="P2"
      choices={[{ label: "P2", value: "P2" }]}
      face="P2"
      label="Priority of CORE-1"
      marks={{}}
      onChoose={() => {}}
      shortcutKeys={["p", "Shift+P"]}
      triggerClassName=""
    >
      <span>P2</span>
    </ChoiceMenu>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Priority of CORE-1 P2" });
    expect(trigger).toHaveAttribute("aria-keyshortcuts", "P Shift+P");
    expect(keyHintOf(trigger)).toBe("p");
    // The key is CSS's to draw, so the words on the trigger are the value it shows alone, and
    // its name holds them (WCAG 2.5.3, Label in Name).
    expect(trigger).toHaveTextContent(/^P2$/);
    view.rerender(
      <ChoiceMenu
        choices={CHOICES}
        face="P2"
        label="Priority of CORE-1"
        marks={{}}
        onChoose={() => {}}
        triggerClassName=""
      >
        <span>P2</span>
      </ChoiceMenu>
    );
    expect(trigger).not.toHaveAttribute("aria-keyshortcuts");
    expect(trigger.querySelector("kbd")).toBeNull();
  } finally {
    view.unmount();
  }
});
