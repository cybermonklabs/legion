import { expect, spyOn, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { GoingPlanAnswer } from "../../api/types";
import { goingState } from "./going-store";
import {
  BoxFrame,
  BoxRow,
  boxHint,
  boxWords,
  ConversationHeader,
  type PanelSize,
} from "./RunControls";

// The mode control (design v3, state 12): a small text button at the left of the box that names
// the mode the next run takes, and its menu of the three modes, each with one line on what it is
// for and no model and no number. It is a menu button: the keys move in its menu, a choice puts
// the cursor back in the box, and Escape closes the menu alone, never the bar around it.

/** The box row as the bar and the panel draw it, inside a dialog that records each key it hears,
 *  as the bar's dialog closes on Escape. */
function renderRow(): string[] {
  const heard: string[] = [];
  render(
    <div aria-label="Search" onKeyDown={(event) => heard.push(event.key)} role="dialog">
      <BoxRow
        box={
          <BoxFrame chip="CORE-1" chipTitle="CORE-1">
            <input aria-label="Ask the command bar" type="text" />
          </BoxFrame>
        }
        hint={undefined}
        onStop={() => {}}
      />
    </div>
  );
  return heard;
}

test("the mode control names the mode at the left of the box, and its menu says what each is for", async () => {
  const user = userEvent.setup();
  renderRow();
  const row = screen.getByTestId("going-level").parentElement as HTMLElement;
  // The control comes first in the row, before the context chip and the box.
  expect(row.firstElementChild).toBe(screen.getByTestId("going-level"));
  const button = screen.getByTestId("going-level-button");
  expect(button).toHaveTextContent("Standard ▾");
  expect(button).toHaveAttribute("aria-haspopup", "menu");
  expect(button.getAttribute("title")).toContain("A change applies from the next message.");

  await user.click(button);
  const menu = screen.getByRole("menu", { name: "Mode" });
  const items = within(menu).getAllByRole("menuitemradio");
  expect(items.map((item) => item.textContent?.replace("✓", ""))).toEqual([
    "Fast — quick lookups",
    "Standard — most questions",
    "Deep — big changes and reports; can take minutes",
  ]);
  expect(items.map((item) => item.getAttribute("aria-checked"))).toEqual([
    "false",
    "true",
    "false",
  ]);
  // The menu opens with the focus on the mode that is on.
  expect(items[1]).toHaveFocus();

  // A choice sets the mode the next run takes, and puts the cursor back in the box.
  await user.click(within(menu).getByRole("menuitemradio", { name: /^Deep/ }));
  expect(goingState().level).toBe("deep");
  expect(screen.queryByRole("menu")).toBeNull();
  expect(button).toHaveTextContent("Deep ▾");
  expect(screen.getByRole("textbox", { name: "Ask the command bar" })).toHaveFocus();
});

test("the keys move in the mode menu, and Escape closes it without closing the bar", async () => {
  const user = userEvent.setup();
  const heard = renderRow();
  const button = screen.getByTestId("going-level-button");
  button.focus();

  // Down opens the menu on the mode that is on; Down and Up move in it, wrapping.
  await user.keyboard("{ArrowDown}");
  expect(screen.getByRole("menuitemradio", { name: /^Standard/ })).toHaveFocus();
  await user.keyboard("{ArrowDown}");
  expect(screen.getByRole("menuitemradio", { name: /^Deep/ })).toHaveFocus();
  await user.keyboard("{ArrowDown}");
  expect(screen.getByRole("menuitemradio", { name: /^Fast/ })).toHaveFocus();
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("menu")).toBeNull();
  expect(button).toHaveFocus();
  expect(heard).not.toContain("Escape");
  expect(goingState().level).toBe("standard");

  // Enter on a mode chooses it.
  await user.keyboard("{Enter}");
  await user.keyboard("{ArrowUp}");
  expect(screen.getByRole("menuitemradio", { name: /^Fast/ })).toHaveFocus();
  await user.keyboard("{Enter}");
  expect(goingState().level).toBe("fast");
  expect(screen.queryByRole("menu")).toBeNull();
  expect(screen.getByRole("textbox", { name: "Ask the command bar" })).toHaveFocus();
});

// The box beside the mode button is taller than the button, so a menu that opened just under the
// button covered the box's lower-left corner (review a10): it opens clear of the whole row.
test("the mode menu opens a few pixels under the whole box row, never over the box's corner", async () => {
  const user = userEvent.setup();
  renderRow();
  const button = screen.getByTestId("going-level-button");
  const row = screen.getByTestId("going-level").parentElement as HTMLElement;
  const rect = (top: number, bottom: number): DOMRect => ({
    bottom,
    height: bottom - top,
    left: 20,
    right: 120,
    toJSON: () => ({}),
    top,
    width: 100,
    x: 20,
    y: top,
  });
  // The button is 28 px tall and the box, and so the row, 38: the row ends 3 px under the button.
  const boxes = spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (
    this: HTMLElement
  ) {
    return this === button ? rect(100, 128) : this === row ? rect(95, 133) : rect(0, 0);
  });
  try {
    await user.click(button);
    expect(screen.getByRole("menu", { name: "Mode" }).style.top).toBe("137px");
  } finally {
    boxes.mockRestore();
  }
});

// The mode button keeps one width whatever the mode is called, so the box beside it never moves
// when the mode changes (design v3, state 12), and the focused mode is filled and ringed in the
// page's blue, never the browser's black outline.
test("the mode button keeps one width, and the menu's focused mode wears the page's ring", async () => {
  const user = userEvent.setup();
  renderRow();
  const button = screen.getByTestId("going-level-button");
  expect(button.className.split(" ")).toContain("w-20");
  await user.click(button);
  for (const item of screen.getAllByRole("menuitemradio")) {
    expect(item.className.split(" ")).toEqual(
      expect.arrayContaining(["outline-none", "focus-visible:ring-2", "focus:bg-slate-100"])
    );
  }
});

/** A plan that waits, read with no rows; `withdrawn` is a read that says Dispatch withdrew it. */
const readPlan: GoingPlanAnswer = {
  mode: "ask",
  plan: 1,
  rows: [],
  run: "r-1",
  state: "waiting",
  summary: "Page the on-call.",
  version: "v1",
};
const withdrawnPlan: GoingPlanAnswer = { ...readPlan, withdrawn: true };

// While a plan waits, the hint under the box says what Enter does (design v3, Keys; review N1):
// in the empty box it approves, which the hint says wherever the card's Approve has scrolled to,
// and with words in the box it sends them instead; before the plan is read it does nothing yet,
// and once that read failed the hint says why, as the card does, not that it is being read. While
// the run's record still says it waits on a plan whose read says Dispatch withdrew it, Enter does
// nothing, with words in the box or none, and the hint says so; once the record says the run
// ended, there is no hint.
test("the hint under the box says Enter approves a plan that waits, sends words instead of approving, and does nothing on a plan Dispatch withdrew", () => {
  const read = { answering: { plan: undefined, unreadable: undefined } };
  const waits = { ...read, planWaits: true, runWaits: true };
  expect(boxHint(waits, "")).toBe("↵ Approve");
  expect(boxHint(waits, "  ")).toBe("↵ Approve");
  expect(boxHint(waits, "keep it")).toBe("↵ sends your words instead of approving");
  expect(boxHint({ ...read, planWaits: false, runWaits: true }, "")).toBe("Reading the plan…");
  expect(boxHint({ ...read, planWaits: false, runWaits: false }, "")).toBeUndefined();
  const failed = {
    answering: {
      plan: undefined,
      unreadable: {
        message: "The run's plan could not be read.",
        retry: () => {},
        retrying: false,
      },
    },
  };
  expect(boxHint({ ...failed, planWaits: false, runWaits: true }, "")).toBe(
    "The run's plan could not be read."
  );
  expect(boxHint({ ...failed, planWaits: false, runWaits: false }, "")).toBeUndefined();
  const shown = { answering: { plan: withdrawnPlan, unreadable: undefined } };
  expect(boxHint({ ...shown, planWaits: false, runWaits: true }, "")).toBe(
    "↵ does nothing: Dispatch withdrew this plan"
  );
  expect(boxHint({ ...shown, planWaits: false, runWaits: true }, "keep it")).toBe(
    "↵ does nothing: Dispatch withdrew this plan"
  );
  expect(boxHint({ ...shown, planWaits: false, runWaits: false }, "")).toBeUndefined();
});

// What the box says it does now (design v2, rule 5): before a conversation is open, to search or
// ask; while the run's record says it waits on its plan - read and waiting, or not read yet - to
// say what to change; while that record says it waits on a plan whose read says Dispatch withdrew
// it, that Dispatch withdrew it, as Enter then does nothing; and once the run's record says it
// ended, to answer the agent's question or to ask a follow-up.
test("the box says what Enter does now, and that Dispatch withdrew a plan the run's record still waits on", () => {
  const words = (
    plan: GoingPlanAnswer | undefined,
    { asked = false, open = true, planWaits = false, runWaits = false, search = true } = {}
  ) =>
    boxWords({
      asked,
      asking: { answering: { plan, unreadable: undefined }, planWaits, runWaits },
      open,
      search,
    });
  expect(words(undefined, { open: false })).toBe("Search or ask…");
  expect(words(undefined, { open: false, search: false })).toBe("Ask…");
  expect(words(readPlan, { planWaits: true, runWaits: true })).toBe("Say what to change…");
  expect(words(undefined, { runWaits: true })).toBe("Say what to change…");
  expect(words(withdrawnPlan, { runWaits: true })).toBe("Dispatch withdrew this plan");
  expect(words(withdrawnPlan)).toBe("Ask a follow-up…");
  expect(words(withdrawnPlan, { asked: true })).toBe("Answer…");
  expect(words(undefined)).toBe("Ask a follow-up…");
});

// The hint stands under the box, from the box's left edge (design v3, Keys: "the hint under the
// box says so"; review R2), not under the mode button: it is indented by that button's width and
// the row's gap after it, 5 rem and 0.5 rem, which are the two the row holds.
test("the hint under the box starts at the box's left edge, past the mode button and the row's gap", () => {
  render(
    <BoxRow
      box={
        <BoxFrame chip="CORE-1" chipTitle="CORE-1">
          <input aria-label="Ask the command bar" type="text" />
        </BoxFrame>
      }
      hint="↵ Approve"
      onStop={() => {}}
    />
  );
  const hint = screen.getByTestId("going-box-hint");
  expect(hint).toHaveTextContent("↵ Approve");
  expect(screen.getByTestId("going-level-button")).toHaveClass("w-20");
  expect(screen.getByTestId("going-level").parentElement).toHaveClass("gap-2");
  expect(hint).toHaveClass("ps-22");
});

// A project's chip shows its key alone, as the design draws a chip, so it stays whole beside
// Stop in the panel; a screen reader still hears "the project OPS", and the tooltip says it.
test("a project's chip shows its key, and says the project to a screen reader and on hover", () => {
  render(
    <BoxFrame chip="the project OPS" chipTitle="Acting on the project OPS">
      <input aria-label="Ask the command bar" type="text" />
    </BoxFrame>
  );
  const chip = screen.getByTestId("going-box-chip");
  expect(chip.textContent).toBe("the project OPS");
  expect(chip.querySelector(".sr-only")?.textContent).toBe("the project ");
  expect(chip).toHaveAttribute("title", "Acting on the project OPS");
});

// The header is one set over the page and at the side (design v3, states 2 and 10): History,
// the move as an icon with its words as its tooltip, and Close as ×, the same buttons in both.
// While History shows, the header names it, with a back arrow, and History is pressed.
test("the header is one set over the page and at the side, and names History while it shows", () => {
  const header = (move: "Keep at the side" | "Open over the page", history: boolean) =>
    render(
      <ConversationHeader
        history={history}
        move={move}
        onClose={() => {}}
        onHistory={() => {}}
        onMove={() => {}}
        onNew={() => {}}
      />
    );
  const over = header("Keep at the side", false);
  const overClose = within(over.container).getByRole("button", { name: "Close" });
  const overMove = within(over.container).getByRole("button", { name: "Keep at the side" });
  const side = header("Open over the page", false);
  const sideClose = within(side.container).getByRole("button", { name: "Close" });
  const sideMove = within(side.container).getByRole("button", { name: "Open over the page" });
  expect(overClose.className).toBe(sideClose.className);
  expect(overClose.textContent).toBe("×");
  expect(sideClose.textContent).toBe("×");
  expect(overMove.className).toBe(sideMove.className);
  expect(overMove).toHaveAttribute("title", "Keep at the side");

  const listing = header("Keep at the side", true);
  expect(within(listing.container).getByTestId("going-history-heading")).toHaveTextContent(
    "History"
  );
  expect(within(listing.container).getByRole("button", { name: "Back" })).toBeInTheDocument();
  expect(within(listing.container).getByTestId("going-history-button")).toHaveAttribute(
    "aria-pressed",
    "true"
  );
});

// The panel at the side widens to the app's content area and docks back (A6) with one icon button
// between the move and Close, named for the press it makes; the bar, which has no width of its
// own, is given no size and draws none.
test("the header draws a width button only where it is given a size, named for the press it makes", async () => {
  const user = userEvent.setup();
  let toggled = 0;
  const header = (size?: PanelSize) =>
    render(
      <ConversationHeader
        history={false}
        move="Open over the page"
        onClose={() => {}}
        onHistory={() => {}}
        onMove={() => {}}
        onNew={() => {}}
        size={size}
      />
    );
  const bar = header();
  expect(within(bar.container).queryByTestId("going-size")).toBeNull();
  bar.unmount();

  const docked = header({ onToggle: () => (toggled += 1), wide: false });
  const widen = within(docked.container).getByRole("button", { name: "Widen the panel" });
  expect(widen).toHaveAttribute("title", "Widen the panel");
  expect(widen.textContent).toBe("");
  expect(widen.querySelector("svg")).not.toBeNull();
  // It stands between the move and Close, in the same bordered square.
  const move = within(docked.container).getByRole("button", { name: "Open over the page" });
  const close = within(docked.container).getByRole("button", { name: "Close" });
  expect(move.nextElementSibling).toBe(widen);
  expect(widen.nextElementSibling).toBe(close);
  expect(widen.className).toBe(move.className);
  await user.click(widen);
  expect(toggled).toBe(1);
  docked.unmount();

  const wide = header({ onToggle: () => (toggled += 1), wide: true });
  const dock = within(wide.container).getByRole("button", { name: "Dock at the side" });
  expect(dock).toHaveAttribute("title", "Dock at the side");
  await user.click(dock);
  expect(toggled).toBe(2);
});
