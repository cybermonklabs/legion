import { expect, mock, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { BarActionProps } from "./BarAction";
import { MoreMenu } from "./MoreMenu";

const ICON = <svg aria-hidden="true" />;

function action(label: string, more: Partial<BarActionProps> = {}): BarActionProps {
  return { icon: ICON, label, onPress: () => undefined, ...more };
}

test("More sets its own data-control, is named More, and draws its glyph alone, quiet", () => {
  const view = render(<MoreMenu actions={[action("Mark all read")]} />);
  try {
    const more = screen.getByRole("button", { name: "More" });
    expect(more).toHaveAttribute("data-control", "more-menu");
    expect(more).toHaveAttribute("aria-haspopup", "dialog");
    expect(more.textContent).toBe("");
    expect(more.querySelector("svg")).not.toBeNull();
    expect(more).toHaveClass("bg-transparent");
  } finally {
    view.unmount();
  }
});

test("its items are menu items, a check item where the action carries a checked state, and the arrow keys walk them", async () => {
  const user = userEvent.setup();
  const view = render(
    <MoreMenu
      actions={[
        action("Mark all read"),
        action("Show Icebox", { checked: false }),
        action("Lanes", { checked: true }),
      ]}
    />
  );
  try {
    await user.click(screen.getByRole("button", { name: "More" }));
    const menu = screen.getByRole("menu", { name: "More" });
    const mark = within(menu).getByRole("menuitem", { name: "Mark all read" });
    const icebox = within(menu).getByRole("menuitemcheckbox", { name: "Show Icebox" });
    const lanes = within(menu).getByRole("menuitemcheckbox", { name: "Lanes" });
    expect(icebox).toHaveAttribute("aria-checked", "false");
    expect(lanes).toHaveAttribute("aria-checked", "true");
    // The menu opens with the focus on its first item, and the arrows walk the rest.
    expect(mark).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(icebox).toHaveFocus();
    await user.keyboard("{ArrowDown}");
    expect(lanes).toHaveFocus();
    await user.keyboard("{ArrowUp}");
    expect(icebox).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a press on an item shuts the menu, puts the reader back on More, and calls the page's handler; a check item leaves it open", async () => {
  const user = userEvent.setup();
  const marked = mock(() => {});
  const flipped = mock(() => {});
  const view = render(
    <MoreMenu
      actions={[
        action("Mark all read", { onPress: marked }),
        action("Show Icebox", { checked: false, onPress: flipped }),
      ]}
    />
  );
  try {
    const more = screen.getByRole("button", { name: "More" });
    await user.click(more);
    await user.click(screen.getByRole("menuitemcheckbox", { name: "Show Icebox" }));
    expect(flipped).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("menu", { name: "More" })).toBeTruthy();
    await user.click(screen.getByRole("menuitem", { name: "Mark all read" }));
    expect(marked).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(more).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("More takes its size from the page", () => {
  const view = render(<MoreMenu actions={[action("Mark all read")]} size="compact" />);
  try {
    expect(screen.getByRole("button", { name: "More" })).toHaveClass("md:min-h-8");
    view.rerender(<MoreMenu actions={[action("Mark all read")]} />);
    expect(screen.getByRole("button", { name: "More" })).not.toHaveClass("md:min-h-8");
  } finally {
    view.unmount();
  }
});
