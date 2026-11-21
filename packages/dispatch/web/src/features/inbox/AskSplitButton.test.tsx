import { expect, mock, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { type ReactNode, useState } from "react";

import { NoTooltips } from "../issue-page/NoTooltips";
import { type AskMenuItem, AskSplitButton } from "./AskSplitButton";

function SplitButton({
  actionRefused = false,
  groups,
}: {
  actionRefused?: boolean;
  groups: readonly (readonly AskMenuItem[])[];
}): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <form onSubmit={(event) => event.preventDefault()}>
      <AskSplitButton
        action="Answer"
        actionRefused={actionRefused}
        groups={groups}
        menuOpen={open}
        onMenuOpenChange={setOpen}
      />
    </form>
  );
}

function item(id: string, label: string, hotkey: string, run = () => {}): AskMenuItem {
  return { hotkey, id, label, run };
}

/** The open menu's groups, each the labels and keys of its items in their order. */
function menuGroups(): { key: string | null; label: string }[][] {
  const menu = screen.getByRole("menu", { name: "More actions" });
  return Array.from(menu.querySelectorAll("[data-ask-menu] > div"), (group) =>
    Array.from(group.querySelectorAll<HTMLElement>("[role=menuitem]"), (entry) => ({
      key: entry.getAttribute("aria-keyshortcuts"),
      label: entry.querySelector("span")?.textContent ?? "",
    }))
  );
}

test("the chevron opens the card's other actions in their groups, each item showing its key", async () => {
  const user = userEvent.setup();
  const view = render(
    <SplitButton
      groups={[
        [item("rewrite", "Rewrite", "r"), item("ask-back", "Ask back", "b")],
        [],
        [item("define", "Define selected words", "d")],
      ]}
    />
  );
  try {
    await user.click(screen.getByRole("button", { name: "More actions" }));
    // An empty group draws nothing, not even its rule.
    expect(menuGroups()).toEqual([
      [
        { key: "r", label: "Rewrite" },
        { key: "b", label: "Ask back" },
      ],
      [{ key: "d", label: "Define selected words" }],
    ]);
    const menu = screen.getByRole("menu", { name: "More actions" });
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((entry) => entry.textContent)
    ).toEqual(["Rewriter", "Ask backb", "Define selected wordsd"]);
  } finally {
    view.unmount();
  }
});

test("the chevron opens the menu while the action cannot be pressed", async () => {
  const user = userEvent.setup();
  const view = render(<SplitButton actionRefused groups={[[item("ask-back", "Ask back", "b")]]} />);
  try {
    expect(screen.getByRole("button", { name: "Answer" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "More actions" }));
    expect(screen.getByRole("menuitem", { name: "Ask back" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("an item's key runs it from the open menu, and a refused item runs on neither", async () => {
  const user = userEvent.setup();
  const askBack = mock(() => {});
  const define = mock(() => {});
  const view = render(
    <SplitButton
      groups={[
        [item("ask-back", "Ask back", "b", askBack)],
        [{ ...item("define", "Define selected words", "d", define), refused: "Select words" }],
      ]}
    />
  );
  try {
    await user.click(screen.getByRole("button", { name: "More actions" }));
    const refused = screen.getByRole("menuitem", { name: "Define selected words" });
    expect(refused).toHaveAttribute("aria-disabled", "true");
    expect(refused).toHaveAttribute("title", "Select words");
    await user.keyboard("d");
    await user.click(refused);
    expect(define).not.toHaveBeenCalled();

    await user.keyboard("b");
    expect(askBack).toHaveBeenCalledTimes(1);
    // Running an item closes the menu.
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

// Inside `NoTooltips` the menu draws no `title`: a refused item says why it cannot run as its
// description, read after its name, and the chevron is named by its label alone. Outside it the
// item's reason and the chevron's name are their tooltips, and the item has no description.
test("inside NoTooltips a refused item is described by its reason and has no title", async () => {
  const user = userEvent.setup();
  const reason = "Select words in the question first";
  const groups = [[{ ...item("define", "Define selected words", "d"), refused: reason }]];
  for (const inside of [true, false]) {
    const button = <SplitButton groups={groups} />;
    const view = render(inside ? <NoTooltips>{button}</NoTooltips> : button);
    try {
      const chevron = screen.getByRole("button", { name: "More actions" });
      await user.click(chevron);
      const refused = screen.getByRole("menuitem", { name: "Define selected words" });
      const describedBy = refused.getAttribute("aria-describedby");
      if (inside) {
        expect(chevron).not.toHaveAttribute("title");
        expect(refused).not.toHaveAttribute("title");
        expect(describedBy).not.toBeNull();
        expect(document.getElementById(describedBy ?? "")?.textContent).toBe(reason);
      } else {
        expect(chevron).toHaveAttribute("title", "More actions");
        expect(refused).toHaveAttribute("title", reason);
        expect(describedBy).toBeNull();
      }
    } finally {
      view.unmount();
    }
  }
});
