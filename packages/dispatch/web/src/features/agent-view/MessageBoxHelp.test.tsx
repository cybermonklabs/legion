import { expect, test } from "bun:test";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";

import { MessageBoxHelp } from "./MessageBoxHelp";

/** The question mark as its host drives it: the host owns the panel, as the footer row does. */
function Host() {
  const [open, setOpen] = useState(false);
  return <MessageBoxHelp onOpenChange={setOpen} open={open} />;
}

// R6. The page prints no rule, so everything a person has to know about the message box is
// behind this one question mark: what `!` does, where a command runs, and what this view keeps
// of the conversation.
test("the question mark opens the panel, which says what ! does, where a command runs and what is kept", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByRole("button", { name: "What the message box does" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByTestId("agent-help-popover")).toBeNull();

    await user.click(trigger);

    const panel = screen.getByRole("dialog", { name: "What the message box does" });
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    const said = panel.textContent ?? "";
    expect(said).toContain("! at the start of a message runs the rest as a shell command");
    expect(said).toContain("A BTW runs nothing");
    expect(said).toContain("/ at the start of a message runs one of the session's own commands");
    expect(said).toContain("A command runs nowhere else.");
    expect(said).toContain("relayed while this page is open and are not stored");
    expect(said).toContain(
      "Messages sent to it through Dispatch, and its Dispatch replies, are kept"
    );
    // The panel says what is true; it drives nothing in the session.
    expect(panel.querySelectorAll("button, input, select, a")).toHaveLength(0);
  } finally {
    view.unmount();
  }
});

test("Escape closes the panel and leaves the focus on the question mark", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    const trigger = screen.getByTestId("agent-help");
    await user.click(trigger);
    expect(screen.getByTestId("agent-help-popover")).toBeTruthy();

    await user.keyboard("{Escape}");

    expect(screen.queryByTestId("agent-help-popover")).toBeNull();
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(trigger);
  } finally {
    view.unmount();
  }
});

// A press on the page behind it is a press on the page, not on the panel: the panel goes, and a
// press inside it - on the text a person is reading - leaves it standing.
test("a pointer press outside closes the panel, one inside leaves it open", async () => {
  const user = userEvent.setup();
  const view = render(<Host />);
  try {
    await user.click(screen.getByTestId("agent-help"));
    const panel = screen.getByTestId("agent-help-popover");

    await user.click(panel);
    expect(screen.queryByTestId("agent-help-popover")).toBeTruthy();

    await user.click(document.body);
    expect(screen.queryByTestId("agent-help-popover")).toBeNull();
  } finally {
    view.unmount();
  }
});
