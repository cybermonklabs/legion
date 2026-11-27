import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { ViewPanel } from "./ViewPanel";

function Host() {
  const [open, setOpen] = useState(false);
  return (
    <ViewPanel label="Save view" onOpenChange={setOpen} open={open}>
      {() => <p>Name the view.</p>}
    </ViewPanel>
  );
}

test("a view panel's trigger sets its own data-control, view-panel, and opens its panel", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByRole("button", { name: "Save view" });
    expect(trigger).toHaveAttribute("data-control", "view-panel");
    // The row keeps one `panel-trigger`, the one over the narrowings.
    expect(document.querySelector('[data-control="panel-trigger"]')).toBeNull();
    await user.click(trigger);
    expect(screen.getByRole("dialog", { name: "Save view" })).toHaveTextContent("Name the view.");
  } finally {
    view.unmount();
  }
});
