import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { PanelTrigger } from "./PanelTrigger";

function Host({ count }: { count?: number }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <PanelTrigger count={count} label="Columns" onOpenChange={setOpen} open={open}>
        {(close) => (
          <button onClick={close} type="button">
            Done
          </button>
        )}
      </PanelTrigger>
      <button type="button">Elsewhere</button>
    </>
  );
}

test("the panel is on the page only while it is open, and the trigger names what it opens", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByRole("button", { name: "Columns" });
    expect(trigger).toHaveAttribute("data-control", "panel-trigger");
    expect(trigger).toHaveAttribute("aria-haspopup", "dialog");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await user.click(trigger);
    expect(screen.getByRole("dialog", { name: "Columns" })).toBeTruthy();
    expect(trigger).toHaveAttribute("aria-expanded", "true");
  } finally {
    view.unmount();
  }
});

test("a press tells the host to open, and the next one to close: the host owns which is open", async () => {
  const user = userEvent.setup();
  const presses: boolean[] = [];
  const view = render(
    <PanelTrigger label="Columns" onOpenChange={(next) => presses.push(next)} open={false}>
      {() => <span>Nothing here.</span>}
    </PanelTrigger>
  );
  try {
    await user.click(screen.getByRole("button", { name: "Columns" }));
    expect(presses).toEqual([true]);

    view.rerender(
      <PanelTrigger label="Columns" onOpenChange={(next) => presses.push(next)} open>
        {() => <span>Nothing here.</span>}
      </PanelTrigger>
    );
    expect(screen.getByRole("button", { name: "Columns" })).toHaveAttribute(
      "aria-expanded",
      "true"
    );
    await user.click(screen.getByRole("button", { name: "Columns" }));
    expect(presses).toEqual([true, false]);
  } finally {
    view.unmount();
  }
});

test("the close the panel is handed shuts it and leaves the reader on the trigger", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByRole("button", { name: "Columns" });
    await user.click(trigger);
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("a count given is read on the trigger, the way a multi-select counts its picks", () => {
  const view = render(<Host count={2} />);
  try {
    expect(screen.getByRole("button", { name: "Columns · 2" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a title is the accessible name of both the trigger and the panel it opens", async () => {
  const user = userEvent.setup();
  const view = render(
    <PanelTrigger
      label={<span aria-hidden="true">▾</span>}
      onOpenChange={() => undefined}
      open={false}
      title="Filter Status"
    >
      {() => <span>Nothing here.</span>}
    </PanelTrigger>
  );
  try {
    expect(screen.getByRole("button", { name: "Filter Status" })).toBeTruthy();
    view.rerender(
      <PanelTrigger
        label={<span aria-hidden="true">▾</span>}
        onOpenChange={() => undefined}
        open
        title="Filter Status"
      >
        {() => <span>Nothing here.</span>}
      </PanelTrigger>
    );
    await user.click(screen.getByRole("button", { name: "Filter Status" }));
    expect(screen.getByRole("dialog", { name: "Filter Status" })).toBeTruthy();
  } finally {
    view.unmount();
  }
});

test("a trigger given a control of its own carries that name, and the panel stays this one", () => {
  const view = render(
    <PanelTrigger
      control="column-filter"
      label="Status"
      onOpenChange={() => undefined}
      open={false}
    >
      {() => <span>Nothing here.</span>}
    </PanelTrigger>
  );
  try {
    // A column filter, a columns menu and a display menu are each this button under a name of
    // their own, so a row finds the one it means without a second definition of the button.
    expect(screen.getByRole("button", { name: "Status" })).toHaveAttribute(
      "data-control",
      "column-filter"
    );
    expect(document.querySelector('[data-control="panel-trigger"]')).toBeNull();
  } finally {
    view.unmount();
  }
});
