import { expect, test } from "bun:test";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { PanelTrigger } from "./PanelTrigger";
import { useNestedPanels } from "./useNestedPanels";

/** A filter panel holding two lists of its own and one more control, as the search bar's holds
 *  its Project and Status lists ahead of the Author box. */
function FilterPanel() {
  const [open, setOpen] = useState(false);
  const inner = useNestedPanels<"project" | "status">(open);
  return (
    <>
      <PanelTrigger label="Filter" onOpenChange={setOpen} open={open} stacked>
        {() => (
          <>
            <PanelTrigger label="Project" {...inner("project")}>
              {() => (
                <>
                  <button type="button">CORE</button>
                  <button type="button">INFRA</button>
                </>
              )}
            </PanelTrigger>
            <PanelTrigger label="Status" {...inner("status")}>
              {() => <button type="button">Done</button>}
            </PanelTrigger>
            <button type="button">Author</button>
          </>
        )}
      </PanelTrigger>
      <button type="button">Elsewhere</button>
    </>
  );
}

test("the filter panel keeps one list of its own open at a time", async () => {
  const user = userEvent.setup();
  const view = render(<FilterPanel />);
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    const panel = screen.getByRole("dialog", { name: "Filter" });
    await user.click(within(panel).getByRole("button", { name: "Project" }));
    expect(screen.getByRole("dialog", { name: "Project" })).toBeTruthy();
    await user.click(within(panel).getByRole("button", { name: "Status" }));
    expect(screen.queryByRole("dialog", { name: "Project" })).not.toBeInTheDocument();
    expect(screen.getByRole("dialog", { name: "Status" })).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Filter" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a press in a list of the panel's own is a press in the panel: it closes neither", async () => {
  const user = userEvent.setup();
  const view = render(<FilterPanel />);
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await user.click(screen.getByRole("button", { name: "Project" }));
    await user.click(
      within(screen.getByRole("dialog", { name: "Project" })).getByRole("button", { name: "CORE" })
    );
    expect(screen.getByRole("dialog", { name: "Project" })).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Filter" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a press outside every panel closes the list and the filter panel together", async () => {
  const user = userEvent.setup();
  const view = render(<FilterPanel />);
  try {
    await user.click(screen.getByRole("button", { name: "Filter" }));
    await user.click(screen.getByRole("button", { name: "Project" }));
    await user.click(screen.getByRole("button", { name: "Elsewhere" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});

test("after Shift+Tab out of a list, Escape closes the filter panel and puts the reader on its trigger", async () => {
  const user = userEvent.setup();
  const view = render(<FilterPanel />);
  try {
    const trigger = screen.getByRole("button", { name: "Filter" });
    await user.click(trigger);
    const panel = screen.getByRole("dialog", { name: "Filter" });
    await user.click(within(panel).getByRole("button", { name: "Project" }));
    const list = screen.getByRole("dialog", { name: "Project" });
    expect(within(list).getByRole("button", { name: "CORE" })).toHaveFocus();
    // Out of the list from its first control: the list closes, and the reader is on its trigger.
    await user.tab({ shift: true });
    expect(screen.queryByRole("dialog", { name: "Project" })).not.toBeInTheDocument();
    expect(within(panel).getByRole("button", { name: "Project" })).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a list the focus has left never holds the filter panel open: Escape closes both, and the next open starts with no list open", async () => {
  const user = userEvent.setup();
  const view = render(<FilterPanel />);
  try {
    const trigger = screen.getByRole("button", { name: "Filter" });
    await user.click(trigger);
    const panel = screen.getByRole("dialog", { name: "Filter" });
    await user.click(within(panel).getByRole("button", { name: "Project" }));
    // The focus moves on to the panel's own Author box with the list still standing.
    act(() => within(panel).getByRole("button", { name: "Author" }).focus());
    expect(screen.getByRole("dialog", { name: "Project" })).toBeTruthy();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
    await user.click(trigger);
    expect(screen.getByRole("dialog", { name: "Filter" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Project" })).not.toBeInTheDocument();
  } finally {
    view.unmount();
  }
});
