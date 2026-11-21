import { expect, mock, test } from "bun:test";
import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FormEvent } from "react";

import { runFocusFixup } from "../__tests__/focus-fixup";
import { RefusableButton } from "./RefusableButton";

// A keyboard reader who presses a button keeps their place while that press is in flight, so their
// next key reaches the button rather than the page's body. While the press is in flight the button
// says it cannot be pressed, and a press of it does nothing; once it ends, a press works again.
test("a keyboard press keeps the focus while it is in flight, and Enter presses again once it ends", async () => {
  const user = userEvent.setup();
  const onPress = mock(() => {});
  const saveButton = (busy?: string) => (
    <RefusableButton busy={busy} onPress={onPress} refusal={null}>
      Save
    </RefusableButton>
  );
  const view = render(saveButton());
  const stopFocusFixup = runFocusFixup();
  try {
    const save = screen.getByRole("button", { name: "Save" });
    act(() => save.focus());
    await user.keyboard(" ");
    expect(onPress).toHaveBeenCalledTimes(1);

    // The press is in flight: the button names it, says it cannot be pressed, either way a
    // control can say so, and keeps the focus.
    view.rerender(saveButton("Saving…"));
    expect(save).toHaveTextContent("Saving…");
    await waitFor(() => expect(save.matches(":disabled, [aria-disabled='true']")).toBe(true));
    expect(save).toHaveFocus();
    await user.keyboard(" ");
    expect(onPress).toHaveBeenCalledTimes(1);

    view.rerender(saveButton());
    expect(save).toHaveFocus();
    expect(save).not.toHaveAttribute("aria-disabled");
    await user.keyboard("{Enter}");
    expect(onPress).toHaveBeenCalledTimes(2);
  } finally {
    stopFocusFixup();
    view.unmount();
  }
});

// A busy submit button submits its form by no press, a click or a key: the press it names is
// already in flight.
test("a busy submit button keeps the focus and submits its form by neither a click nor a key", async () => {
  const user = userEvent.setup();
  const onSubmit = mock((event: FormEvent<HTMLFormElement>) => event.preventDefault());
  const view = render(
    <form onSubmit={onSubmit}>
      <RefusableButton busy="Saving…" refusal={null} type="submit">
        Save
      </RefusableButton>
    </form>
  );
  try {
    const save = screen.getByRole("button", { name: "Saving…" });
    expect(save).toHaveAttribute("aria-disabled", "true");
    await user.click(save);
    act(() => save.focus());
    await user.keyboard(" ");
    await user.keyboard("{Enter}");
    expect(save).toHaveFocus();
    expect(onSubmit).not.toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});
