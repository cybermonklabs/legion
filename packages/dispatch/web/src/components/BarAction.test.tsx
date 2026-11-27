import { expect, mock, test } from "bun:test";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { primaryButtonBg } from "../theme/classes";
import { BarAction } from "./BarAction";

const ICON = <svg aria-hidden="true" />;

test("an action is a quiet button that sets its own data-control and calls the page's handler", async () => {
  const user = userEvent.setup();
  const onPress = mock(() => {});
  const view = render(<BarAction icon={ICON} label="New document" onPress={onPress} />);
  try {
    const action = screen.getByRole("button", { name: "New document" });
    expect(action).toHaveAttribute("data-control", "bar-action");
    expect(action).toHaveClass("bg-transparent");
    expect(action).not.toHaveClass("border");
    await user.click(action);
    expect(onPress).toHaveBeenCalledTimes(1);
  } finally {
    view.unmount();
  }
});

test("an action given a refusal is held with its refusal as its description, and a press on it does nothing", async () => {
  const user = userEvent.setup();
  const onPress = mock(() => {});
  const view = render(
    <>
      <BarAction
        icon={ICON}
        label="Upload"
        onPress={onPress}
        refusal={{ id: "closed", words: "This issue is closed." }}
      />
      <p id="closed">This issue is closed.</p>
    </>
  );
  try {
    const action = screen.getByRole("button", { name: "Upload" });
    expect(action).toHaveAttribute("aria-disabled", "true");
    expect(action).toHaveAttribute("title", "This issue is closed.");
    expect(action).toHaveAccessibleDescription("This issue is closed.");
    await user.click(action);
    expect(onPress).not.toHaveBeenCalled();
    expect(action).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("the main action is drawn filled, and drawn as its glyph alone it keeps its label as its name", () => {
  const view = render(
    <BarAction filled icon={ICON} label="New message" onPress={() => undefined} />
  );
  try {
    const main = screen.getByRole("button", { name: "New message" });
    for (const name of primaryButtonBg.split(" ")) expect(main).toHaveClass(name);
    expect(main).toHaveTextContent("New message");
    view.rerender(
      <BarAction filled icon={ICON} iconOnly label="New message" onPress={() => undefined} />
    );
    const glyph = screen.getByRole("button", { name: "New message" });
    expect(glyph.textContent).toBe("");
    expect(glyph.querySelector("svg")).not.toBeNull();
  } finally {
    view.unmount();
  }
});

test("an action given a panel is a view-panel trigger that opens it and tells the page", async () => {
  const user = userEvent.setup();
  const onPress = mock(() => {});
  const view = render(
    <BarAction
      icon={ICON}
      label="Save view"
      onPress={onPress}
      panel={(close) => (
        <button onClick={close} type="button">
          Save
        </button>
      )}
    />
  );
  try {
    const trigger = screen.getByRole("button", { name: "Save view" });
    expect(trigger).toHaveAttribute("data-control", "view-panel");
    expect(document.querySelector('[data-control="bar-action"]')).toBeNull();
    await user.click(trigger);
    expect(onPress).toHaveBeenCalledTimes(1);
    const panel = screen.getByRole("dialog", { name: "Save view" });
    await user.click(within(panel).getByRole("button", { name: "Save" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("an action with a panel given a refusal is held with its refusal as its description", async () => {
  const user = userEvent.setup();
  const onPress = mock(() => {});
  const view = render(
    <>
      <BarAction
        icon={ICON}
        label="Save view"
        onPress={onPress}
        panel={() => <p>Name the view</p>}
        refusal={{ id: "closed", words: "This issue is closed." }}
      />
      <p id="closed">This issue is closed.</p>
    </>
  );
  try {
    const trigger = screen.getByRole("button", { name: "Save view" });
    expect(trigger).toHaveAttribute("aria-disabled", "true");
    expect(trigger).toHaveAttribute("title", "This issue is closed.");
    expect(trigger).toHaveAccessibleDescription("This issue is closed.");
    await user.click(trigger);
    expect(onPress).not.toHaveBeenCalled();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(trigger).toHaveFocus();
  } finally {
    view.unmount();
  }
});

test("an action takes its size from the page", () => {
  const view = render(
    <BarAction icon={ICON} label="Upload" onPress={() => undefined} size="compact" />
  );
  try {
    expect(screen.getByRole("button", { name: "Upload" })).toHaveClass("md:min-h-8");
    view.rerender(<BarAction icon={ICON} label="Upload" onPress={() => undefined} />);
    expect(screen.getByRole("button", { name: "Upload" })).not.toHaveClass("md:min-h-8");
  } finally {
    view.unmount();
  }
});
