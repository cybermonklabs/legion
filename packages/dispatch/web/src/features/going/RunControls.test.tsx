import { expect, spyOn, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { GoingPlanAnswer } from "../../api/types";
import { goingState } from "./going-store";
import { BoxFrame, BoxRow, boxHint, ConversationHeader } from "./RunControls";

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

// While a plan waits, the hint under the box says what Enter does (design v3, Keys; review N1):
// in the empty box it approves, which the hint says wherever the card's Approve has scrolled to,
// and with words in the box it sends them instead; before the plan is read it does nothing yet,
// and once that read failed the hint says why, as the card does, not that it is being read. A
// plan read that waits for nobody, a withdrawn run's, has no hint while the run's frames still
// say it waits: the card above shows the plan.
test("the hint under the box says Enter approves a plan that waits, and sends words instead of approving", () => {
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
  const withdrawn: GoingPlanAnswer = {
    mode: "ask",
    plan: 1,
    rows: [],
    run: "r-1",
    state: "waiting",
    summary: "Page the on-call.",
    version: "v1",
    withdrawn: true,
  };
  const shown = { answering: { plan: withdrawn, unreadable: undefined } };
  expect(boxHint({ ...shown, planWaits: false, runWaits: true }, "")).toBeUndefined();
  expect(boxHint({ ...shown, planWaits: false, runWaits: true }, "keep it")).toBeUndefined();
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
